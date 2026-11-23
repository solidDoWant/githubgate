package fakegithub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The Git Database endpoints are backed by the repository's bare git
// repository and git plumbing commands. Quirks copied from GitHub are noted
// where they are handled; "documented" means the behaviour is stated in
// https://docs.github.com/en/rest/git, "observed" means it was confirmed
// against github.com by the owner's live runs.

var (
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// GitHub accepts these tree entry modes:
	// https://docs.github.com/en/rest/git/trees#create-a-tree
	treeModes = map[string]string{"100644": "blob", "100755": "blob", "040000": "tree", "160000": "commit", "120000": "blob"}
)

type gitFailure struct {
	status  int
	message string
}

func (f gitFailure) Error() string { return f.message }

func (s *Server) repoDir(owner, name string) string {
	return filepath.Join(s.gitRoot, owner, name+".git")
}

// runGit runs git in a bare repository. A failure is an error carrying git's
// message, never shown to callers.
func runGit(dir string, stdin []byte, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_DIR=" + dir}, env...)...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return bytes.TrimSpace(stdout.Bytes()), nil
}

func objectType(dir, sha string) (string, bool) {
	if !shaPattern.MatchString(sha) {
		return "", false
	}

	out, err := runGit(dir, nil, nil, "cat-file", "-t", sha)
	if err != nil {
		return "", false
	}

	return string(out), true
}

func readBody(writer http.ResponseWriter, request *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 32<<20)).Decode(into); err != nil {
		writeJSONError(writer, http.StatusBadRequest, "Problems parsing JSON")

		return false
	}

	return true
}

// handleCreateBlob creates a blob from utf-8 or base64 content:
// https://docs.github.com/en/rest/git/blobs#create-a-blob
func (s *Server) handleCreateBlob(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body struct {
		Content  *string `json:"content"`
		Encoding string  `json:"encoding"`
	}
	if !readBody(writer, request, &body) {
		return
	}

	if body.Content == nil {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	content := []byte(*body.Content)

	switch body.Encoding {
	case "", "utf-8":
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(*body.Content)
		if err != nil {
			writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

			return
		}

		content = decoded
	default:
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")

	sha, err := runGit(s.repoDir(owner, name), content, nil, "hash-object", "-w", "--stdin")
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Server Error")

		return
	}

	writeJSON(writer, http.StatusCreated, map[string]any{"sha": string(sha), "url": apiURL(request, owner, name, "git/blobs/"+string(sha))})
}

// handleGetBlob returns the blob as base64 with its size:
// https://docs.github.com/en/rest/git/blobs#get-a-blob
func (s *Server) handleGetBlob(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name, sha := request.PathValue("owner"), request.PathValue("repo"), request.PathValue("sha")
	dir := s.repoDir(owner, name)

	if kind, ok := objectType(dir, sha); !ok || kind != "blob" {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	content, err := catBlob(dir, sha)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Server Error")

		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"sha": sha, "size": len(content), "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString(content),
		"url":     apiURL(request, owner, name, "git/blobs/"+sha),
	})
}

// catBlob reads a blob byte for byte; runGit trims its output.
func catBlob(dir, sha string) ([]byte, error) {
	cmd := exec.Command("git", "cat-file", "blob", sha)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_DIR="+dir)

	return cmd.Output()
}

type treeEntry struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	SHA     *string `json:"sha"`
	Content *string `json:"content"`
}

// handleCreateTree builds a tree, optionally on top of base_tree. An entry
// whose sha is null deletes the path from the base tree:
// https://docs.github.com/en/rest/git/trees#create-a-tree
func (s *Server) handleCreateTree(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body struct {
		BaseTree string      `json:"base_tree"`
		Tree     []treeEntry `json:"tree"`
	}
	if !readBody(writer, request, &body) {
		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")

	sha, err := s.buildTree(s.repoDir(owner, name), body.BaseTree, body.Tree)
	if err != nil {
		var failure gitFailure
		if errors.As(err, &failure) {
			writeJSONError(writer, failure.status, failure.message)
		} else {
			writeJSONError(writer, http.StatusInternalServerError, "Server Error")
		}

		return
	}

	writeJSON(writer, http.StatusCreated, map[string]any{"sha": sha, "url": apiURL(request, owner, name, "git/trees/"+sha), "truncated": false})
}

func (s *Server) buildTree(dir, baseTree string, entries []treeEntry) (string, error) {
	index, err := os.CreateTemp("", "fakegithub-index-")
	if err != nil {
		return "", err
	}

	_ = index.Close()
	_ = os.Remove(index.Name())

	defer func() { _ = os.Remove(index.Name()) }()

	// update-index insists on a work tree; none of the commands touch it.
	env := []string{"GIT_INDEX_FILE=" + index.Name(), "GIT_WORK_TREE=" + dir}

	if baseTree != "" {
		if kind, ok := objectType(dir, baseTree); !ok || kind != "tree" {
			return "", gitFailure{http.StatusUnprocessableEntity, "GitRPC::BadObjectState"}
		}

		if _, err := runGit(dir, nil, env, "read-tree", baseTree); err != nil {
			return "", err
		}
	}

	for _, entry := range entries {
		if err := applyTreeEntry(dir, env, entry); err != nil {
			return "", err
		}
	}

	out, err := runGit(dir, nil, env, "write-tree")
	if err != nil {
		return "", err
	}

	return string(out), nil
}

func applyTreeEntry(dir string, env []string, entry treeEntry) error {
	bad := gitFailure{http.StatusUnprocessableEntity, "Invalid tree info"}

	if entry.Path == "" || strings.HasPrefix(entry.Path, "/") || strings.Contains(entry.Path, "//") {
		return bad
	}

	wantType, okMode := treeModes[entry.Mode]
	if !okMode || (entry.Type != "" && entry.Type != wantType) {
		return bad
	}

	// A null sha with no content is a deletion; only documented for blobs.
	if entry.SHA == nil && entry.Content == nil {
		if _, err := runGit(dir, nil, env, "update-index", "--force-remove", "--", entry.Path); err != nil {
			return err
		}

		return nil
	}

	if wantType == "tree" {
		// Tree entries are only reachable through nested paths here.
		return bad
	}

	sha := ""

	switch {
	case entry.SHA != nil && entry.Content != nil:
		return bad
	case entry.Content != nil:
		out, err := runGit(dir, []byte(*entry.Content), nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}

		sha = string(out)
	default:
		sha = *entry.SHA
		if _, ok := objectType(dir, sha); !ok && entry.Mode != "160000" {
			return gitFailure{http.StatusUnprocessableEntity, "GitRPC::BadObjectState"}
		}
	}

	_, err := runGit(dir, nil, env, "update-index", "--add", "--cacheinfo", entry.Mode+","+sha+","+entry.Path)

	return err
}

type commitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// handleCreateCommit creates a commit. GitHub signs a commit automatically
// only when the request has no author: it is then authored by the App's bot
// user and shows verification.verified true with reason "valid". A request
// with an author gives an unsigned commit (verified false, reason "unsigned").
// https://docs.github.com/en/rest/git/commits#create-a-commit
func (s *Server) handleCreateCommit(writer http.ResponseWriter, request *http.Request, token tokenContext) {
	var body struct {
		Message   string        `json:"message"`
		Tree      string        `json:"tree"`
		Parents   []string      `json:"parents"`
		Author    *commitPerson `json:"author"`
		Committer *commitPerson `json:"committer"`
	}
	if !readBody(writer, request, &body) {
		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")
	dir := s.repoDir(owner, name)

	if body.Message == "" || body.Tree == "" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	if kind, ok := objectType(dir, body.Tree); !ok || kind != "tree" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Tree SHA does not exist")

		return
	}

	args := []string{"commit-tree", body.Tree}

	for _, parent := range body.Parents {
		if kind, ok := objectType(dir, parent); !ok || kind != "commit" {
			writeJSONError(writer, http.StatusUnprocessableEntity, "Parent SHA does not exist")

			return
		}

		args = append(args, "-p", parent)
	}

	signed := body.Author == nil
	now := s.Now().UTC().Format(time.RFC3339)
	author := commitPerson{Date: now}

	if signed {
		author.Name, author.Email = s.appBotIdentity(token)
	} else {
		author = *body.Author
		if author.Date == "" {
			author.Date = now
		}
	}

	committer := author
	if body.Committer != nil {
		committer = *body.Committer
		if committer.Date == "" {
			committer.Date = now
		}
	}

	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + author.Date,
		"GIT_COMMITTER_NAME=" + committer.Name, "GIT_COMMITTER_EMAIL=" + committer.Email, "GIT_COMMITTER_DATE=" + committer.Date,
	}

	out, err := runGit(dir, []byte(body.Message), env, args...)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Server Error")

		return
	}

	sha := string(out)

	s.mu.Lock()
	s.signed[owner+"/"+name+"@"+sha] = signed
	s.mu.Unlock()

	writeJSON(writer, http.StatusCreated, s.commitJSON(request, owner, name, sha))
}

// appBotIdentity is the name and email of the App's bot user, which GitHub
// uses as author of the commits it signs.
func (s *Server) appBotIdentity(token tokenContext) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.installations[token.issued.InstallationID]
	app := s.apps[inst.AppID]

	return app.Name + "[bot]", fmt.Sprintf("%d+%s[bot]@users.noreply.github.com", app.ID, app.Name)
}

func (s *Server) handleGetCommit(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name, sha := request.PathValue("owner"), request.PathValue("repo"), request.PathValue("sha")

	if kind, ok := objectType(s.repoDir(owner, name), sha); !ok || kind != "commit" {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	writeJSON(writer, http.StatusOK, s.commitJSON(request, owner, name, sha))
}

func (s *Server) commitJSON(request *http.Request, owner, name, sha string) map[string]any {
	dir := s.repoDir(owner, name)
	out, _ := runGit(dir, nil, nil, "log", "-1", "--format=%T%n%P%n%an%n%ae%n%aI%n%cn%n%ce%n%cI%n%B", sha)
	lines := strings.SplitN(string(out), "\n", 9)

	for len(lines) < 9 {
		lines = append(lines, "")
	}

	parents := []map[string]any{}

	for _, parent := range strings.Fields(lines[1]) {
		parents = append(parents, map[string]any{"sha": parent})
	}

	s.mu.Lock()
	signed := s.signed[owner+"/"+name+"@"+sha]
	s.mu.Unlock()

	verification := map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil}
	if signed {
		verification = map[string]any{"verified": true, "reason": "valid", "signature": "-----BEGIN PGP SIGNATURE-----\nfake\n-----END PGP SIGNATURE-----", "payload": "fake"}
	}

	return map[string]any{
		"sha":          sha,
		"url":          apiURL(request, owner, name, "git/commits/"+sha),
		"tree":         map[string]any{"sha": lines[0]},
		"parents":      parents,
		"author":       map[string]any{"name": lines[2], "email": lines[3], "date": lines[4]},
		"committer":    map[string]any{"name": lines[5], "email": lines[6], "date": lines[7]},
		"message":      strings.TrimRight(lines[8], "\n"),
		"verification": verification,
	}
}

type refBody struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Force bool   `json:"force"`
}

func refJSON(request *http.Request, owner, name, ref, sha string) map[string]any {
	return map[string]any{"ref": ref, "url": apiURL(request, owner, name, "git/"+ref), "object": map[string]any{"type": "commit", "sha": sha}}
}

// handleCreateRef creates a reference; it fails when the ref exists:
// https://docs.github.com/en/rest/git/refs#create-a-reference
func (s *Server) handleCreateRef(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body refBody
	if !readBody(writer, request, &body) {
		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")
	dir := s.repoDir(owner, name)

	if !strings.HasPrefix(body.Ref, "refs/") || strings.Count(body.Ref, "/") < 2 {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Reference name is invalid")

		return
	}

	if _, err := runGit(dir, nil, nil, "check-ref-format", body.Ref); err != nil {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Reference name is invalid")

		return
	}

	if _, ok := objectType(dir, body.SHA); !ok {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Object does not exist")

		return
	}

	// An empty old value makes update-ref fail when the ref exists.
	if _, err := runGit(dir, nil, nil, "update-ref", body.Ref, body.SHA, strings.Repeat("0", 40)); err != nil {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Reference already exists")

		return
	}

	writeJSON(writer, http.StatusCreated, refJSON(request, owner, name, body.Ref, body.SHA))
}

// handleUpdateRef moves a reference. Without force the move must be a fast
// forward:
// https://docs.github.com/en/rest/git/refs#update-a-reference
func (s *Server) handleUpdateRef(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body refBody
	if !readBody(writer, request, &body) {
		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")
	ref := "refs/" + request.PathValue("ref")
	dir := s.repoDir(owner, name)

	current, err := runGit(dir, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		writeJSONError(writer, http.StatusNotFound, "Reference does not exist")

		return
	}

	if _, ok := objectType(dir, body.SHA); !ok {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Object does not exist")

		return
	}

	if !body.Force {
		if _, err := runGit(dir, nil, nil, "merge-base", "--is-ancestor", string(current), body.SHA); err != nil {
			writeJSONError(writer, http.StatusUnprocessableEntity, "Update is not a fast forward")

			return
		}
	}

	if _, err := runGit(dir, nil, nil, "update-ref", ref, body.SHA); err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Server Error")

		return
	}

	writeJSON(writer, http.StatusOK, refJSON(request, owner, name, ref, body.SHA))
}

// handleDeleteRef deletes a reference, as gh pr merge --delete-branch does.
// GitHub answers 204, or 422 when the ref doesn't exist:
// https://docs.github.com/en/rest/git/refs#delete-a-reference
// The ref may arrive with its slashes escaped ("heads%2Fa%2Fb"); the mux
// decodes the wildcard, as GitHub accepts both forms.
func (s *Server) handleDeleteRef(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name := request.PathValue("owner"), request.PathValue("repo")
	ref := "refs/" + request.PathValue("ref")
	dir := s.repoDir(owner, name)

	if _, err := runGit(dir, nil, nil, "rev-parse", "--verify", "--quiet", ref); err != nil {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Reference does not exist")

		return
	}

	if _, err := runGit(dir, nil, nil, "update-ref", "-d", ref); err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "Server Error")

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRef(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name := request.PathValue("owner"), request.PathValue("repo")
	ref := "refs/" + request.PathValue("ref")

	sha, err := runGit(s.repoDir(owner, name), nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	writeJSON(writer, http.StatusOK, refJSON(request, owner, name, ref, string(sha)))
}

func apiURL(request *http.Request, owner, name, path string) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + request.Host + "/repos/" + owner + "/" + name + "/" + path
}
