package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// maxBlobBytes is GitHub's limit for one file in the Git Database API.
const maxBlobBytes = 100 << 20

const submoduleMode = "160000"

// changedFile is one added, changed or deleted path of a commit.
type changedFile struct {
	path    string
	mode    string
	blob    string
	deleted bool
}

// runPush re-creates the local commits that the remote branch lacks through
// the Git Database API, with no author or committer so GitHub signs them, and
// moves the remote branch once without force.
func runPush(ctx context.Context, env *environment, args []string) error {
	if len(args) > 2 {
		return errors.New("usage: push [<remote> [<branch>]]")
	}

	remote := "origin"
	if len(args) > 0 {
		remote = args[0]
	}

	head, err := gitOutput(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return errors.New("HEAD is detached: check out the branch you want to push")
	}

	branch := strings.TrimPrefix(head, "refs/heads/")
	if len(args) == 2 && args[1] != branch {
		return fmt.Errorf("push publishes the checked-out branch %q, not %q", branch, args[1])
	}

	remoteURL, err := gitOutput(ctx, "config", "--get", "remote."+remote+".url")
	if err != nil {
		return fmt.Errorf("remote %q is not configured", remote)
	}

	slug := parseGitHubRemote(remoteURL)
	if slug == "" {
		return fmt.Errorf("remote %q (%s) does not point at github.com", remote, remoteURL)
	}

	owner, name, _ := strings.Cut(slug, "/")

	oldHead, err := gitOutput(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	base, remoteExists, err := publishBase(ctx, remote, branch)
	if err != nil {
		return err
	}

	if remoteExists {
		if err := gitRun(ctx, "merge-base", "--is-ancestor", base, oldHead); err != nil {
			return fmt.Errorf("HEAD does not contain %s/%s: rebase onto it first", remote, branch)
		}
	}

	rangeOut, err := gitOutput(ctx, "rev-list", "--reverse", "--topo-order", base+".."+oldHead)
	if err != nil {
		return err
	}

	commits := strings.Fields(rangeOut)
	if len(commits) == 0 {
		return printLine(env.stdout, "nothing to push")
	}

	changes := make([][]changedFile, len(commits))

	for index, commit := range commits {
		if changes[index], err = inspectCommit(ctx, commit); err != nil {
			return err
		}
	}

	api, err := newGitHubAPI(ctx, env, owner, name)
	if err != nil {
		return err
	}

	prevCommit := base

	prevTree, err := gitOutput(ctx, "rev-parse", base+"^{tree}")
	if err != nil {
		return err
	}

	for index, commit := range commits {
		tree, err := api.createTree(ctx, prevTree, changes[index], func(sha string) ([]byte, error) {
			return gitBytes(ctx, "cat-file", "blob", sha)
		})
		if err != nil {
			return fmt.Errorf("commit %s: %w", commit, err)
		}

		message, err := signedMessage(ctx, commit)
		if err != nil {
			return err
		}

		sha, err := api.createCommit(ctx, message, tree, prevCommit)
		if err != nil {
			return fmt.Errorf("commit %s: %w", commit, err)
		}

		prevCommit, prevTree = sha, tree
	}

	if err := api.moveBranch(ctx, branch, prevCommit, remoteExists); err != nil {
		return err
	}

	if err := gitRun(ctx, "fetch", "--quiet", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch); err != nil {
		return fmt.Errorf("published, but fetching the new commits failed: %w", err)
	}

	localTree, err := gitOutput(ctx, "rev-parse", oldHead+"^{tree}")
	if err != nil {
		return err
	}

	if localTree != prevTree {
		return printLine(env.stdout, fmt.Sprintf("published %s on %s, but its tree %s differs from the local tree %s; the local branch was left alone", prevCommit, branch, prevTree, localTree))
	}

	if err := gitRun(ctx, "update-ref", head, prevCommit, oldHead); err != nil {
		return fmt.Errorf("published, but moving the local branch failed: %w", err)
	}

	return printLine(env.stdout, fmt.Sprintf("published %d commit(s) to %s; %s is now %s", len(commits), branch, branch, prevCommit))
}

// publishBase fetches the remote branch and returns the commit the range
// starts after. When the remote branch does not exist it is the merge base
// with the remote's default branch.
func publishBase(ctx context.Context, remote, branch string) (string, bool, error) {
	trackingRef := "refs/remotes/" + remote + "/" + branch

	listing, err := gitOutput(ctx, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("list remote %q: %w", remote, err)
	}

	if listing != "" {
		if err := gitRun(ctx, "fetch", "--quiet", remote, "+refs/heads/"+branch+":"+trackingRef); err != nil {
			return "", false, fmt.Errorf("fetch %s %s: %w", remote, branch, err)
		}

		sha, err := gitOutput(ctx, "rev-parse", trackingRef)

		return sha, true, err
	}

	symref, err := gitOutput(ctx, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("list remote %q: %w", remote, err)
	}

	var defaultBranch string

	for _, line := range strings.Split(symref, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			defaultBranch, _, _ = strings.Cut(rest, "\t")
		}
	}

	if defaultBranch == "" {
		return "", false, fmt.Errorf("remote %q has no default branch to start %q from", remote, branch)
	}

	defaultRef := "refs/remotes/" + remote + "/" + defaultBranch
	if err := gitRun(ctx, "fetch", "--quiet", remote, "+refs/heads/"+defaultBranch+":"+defaultRef); err != nil {
		return "", false, fmt.Errorf("fetch %s %s: %w", remote, defaultBranch, err)
	}

	base, err := gitOutput(ctx, "merge-base", defaultRef, "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("HEAD shares no history with %s/%s", remote, defaultBranch)
	}

	return base, false, nil
}

// inspectCommit lists the files a commit changes and refuses what the Git
// Database API cannot express: merge commits, submodules and oversized files.
func inspectCommit(ctx context.Context, commit string) ([]changedFile, error) {
	parents, err := gitOutput(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}

	if len(strings.Fields(parents)) > 2 {
		return nil, fmt.Errorf("commit %s is a merge commit, which push cannot publish; rebase it away", commit)
	}

	raw, err := gitBytes(ctx, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", "--root", commit)
	if err != nil {
		return nil, err
	}

	fields := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")

	var files []changedFile

	for index := 0; index+1 < len(fields); index += 2 {
		// :oldmode newmode oldsha newsha status
		meta := strings.Fields(strings.TrimPrefix(fields[index], ":"))
		if len(meta) < 5 {
			return nil, fmt.Errorf("commit %s: unreadable git diff output", commit)
		}

		oldMode, newMode, newSHA, status := meta[0], meta[1], meta[3], meta[4]
		path := fields[index+1]

		if oldMode == submoduleMode || newMode == submoduleMode {
			return nil, fmt.Errorf("commit %s changes the submodule %q, which push cannot publish", commit, path)
		}

		if status == "D" {
			files = append(files, changedFile{path: path, deleted: true})

			continue
		}

		size, err := gitOutput(ctx, "cat-file", "-s", newSHA)
		if err != nil {
			return nil, err
		}

		if bytes, _ := strconv.ParseInt(size, 10, 64); bytes > maxBlobBytes {
			return nil, fmt.Errorf("commit %s: %q is larger than GitHub's limit of 100 MB for one file", commit, path)
		}

		files = append(files, changedFile{path: path, mode: newMode, blob: newSHA})
	}

	return files, nil
}

// signedMessage is the commit message plus a Co-authored-by trailer for the
// original author.
func signedMessage(ctx context.Context, commit string) (string, error) {
	message, err := gitBytes(ctx, "log", "-1", "--format=%B", commit)
	if err != nil {
		return "", err
	}

	author, err := gitOutput(ctx, "log", "-1", "--format=%an <%ae>", commit)
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "git", "interpret-trailers", "--if-exists", "addIfDifferent", "--trailer", "Co-authored-by: "+author)
	cmd.Stdin = bytes.NewReader(message)

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("add the Co-authored-by trailer: %w", err)
	}

	return string(out), nil
}

func gitBytes(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	out, err := gitBytes(ctx, args...)

	return strings.TrimSpace(string(out)), err
}

func gitRun(ctx context.Context, args ...string) error {
	_, err := gitBytes(ctx, args...)

	return err
}

// githubAPI calls the Git Database endpoints of one repository.
type githubAPI struct {
	client *http.Client
	root   string
	repo   string
	token  string
}

func newGitHubAPI(ctx context.Context, env *environment, owner, name string) (*githubAPI, error) {
	root := env.getenv("GITHUBGATE_GITHUB_API_URL")
	if root == "" {
		root = "https://api.github.com"
	}

	parsed, err := url.Parse(root)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("GITHUBGATE_GITHUB_API_URL %q is not a valid URL", root)
	}

	// The installation token goes to this URL, so it must be encrypted except
	// on the loopback interface (a test double).
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopback(parsed.Hostname())) {
		return nil, errors.New("GITHUBGATE_GITHUB_API_URL must use https")
	}

	response, err := requestToken(ctx, env, tokenRequest{
		Owner:        owner,
		Repositories: []string{name},
		Permissions:  map[string]string{"contents": "write"},
	})
	if err != nil {
		return nil, err
	}

	return &githubAPI{
		client: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		root:  strings.TrimSuffix(parsed.String(), "/"),
		repo:  "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name),
		token: response.Token,
	}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// apiError is a non-success GitHub answer.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub answered %d: %s", e.status, e.message)
}

func (a *githubAPI) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(raw)
	}

	//nolint:forbidigo // The client calls GitHub with the installation token it minted.
	req, err := http.NewRequestWithContext(ctx, method, a.root+a.repo+path, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach GitHub: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		failure := &apiError{status: resp.StatusCode}

		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			failure.message = parsed.Message
		}

		return failure
	}

	if out == nil {
		return nil
	}

	return json.Unmarshal(data, out)
}

// createTree builds the tree of one commit on top of base, reusing blobs the
// repository already has.
func (a *githubAPI) createTree(ctx context.Context, base string, files []changedFile, content func(string) ([]byte, error)) (string, error) {
	if len(files) == 0 {
		return base, nil
	}

	entries := make([]map[string]any, 0, len(files))

	for _, file := range files {
		if file.deleted {
			entries = append(entries, map[string]any{"path": file.path, "mode": "100644", "type": "blob", "sha": nil})

			continue
		}

		sha, err := a.ensureBlob(ctx, file.blob, content)
		if err != nil {
			return "", fmt.Errorf("%s: %w", file.path, err)
		}

		entries = append(entries, map[string]any{"path": file.path, "mode": file.mode, "type": "blob", "sha": sha})
	}

	var out struct {
		SHA string `json:"sha"`
	}

	if err := a.do(ctx, http.MethodPost, "/git/trees", map[string]any{"base_tree": base, "tree": entries}, &out); err != nil {
		return "", err
	}

	return out.SHA, nil
}

func (a *githubAPI) ensureBlob(ctx context.Context, sha string, content func(string) ([]byte, error)) (string, error) {
	err := a.do(ctx, http.MethodGet, "/git/blobs/"+sha, nil, nil)
	if err == nil {
		return sha, nil
	}

	var failure *apiError
	if !errors.As(err, &failure) || failure.status != http.StatusNotFound {
		return "", err
	}

	data, err := content(sha)
	if err != nil {
		return "", err
	}

	var out struct {
		SHA string `json:"sha"`
	}

	body := map[string]string{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64"}
	if err := a.do(ctx, http.MethodPost, "/git/blobs", body, &out); err != nil {
		return "", err
	}

	return out.SHA, nil
}

// createCommit sends no author or committer, so GitHub signs the commit.
func (a *githubAPI) createCommit(ctx context.Context, message, tree, parent string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}

	body := map[string]any{"message": message, "tree": tree, "parents": []string{parent}}
	if err := a.do(ctx, http.MethodPost, "/git/commits", body, &out); err != nil {
		return "", err
	}

	return out.SHA, nil
}

// moveBranch creates or fast-forwards the branch in one request, never with
// force.
func (a *githubAPI) moveBranch(ctx context.Context, branch, sha string, exists bool) error {
	var err error

	if exists {
		err = a.do(ctx, http.MethodPatch, "/git/refs/heads/"+escapePath(branch), map[string]any{"sha": sha, "force": false}, nil)
	} else {
		err = a.do(ctx, http.MethodPost, "/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
	}

	var failure *apiError
	if errors.As(err, &failure) && failure.status == http.StatusUnprocessableEntity {
		return fmt.Errorf("the remote branch %q moved or changed meanwhile, so nothing was changed: fetch, rebase and push again (%s)", branch, failure.message)
	}

	return err
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}
