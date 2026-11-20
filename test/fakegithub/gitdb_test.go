package fakegithub_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

func (f *fixture) post(t *testing.T, token, path string, body any) (int, map[string]any) {
	t.Helper()

	return f.do(t, http.MethodPost, path, token, body)
}

func (f *fixture) blob(t *testing.T, token, content string) string {
	status, out := f.post(t, token, "/repos/acme/one/git/blobs", map[string]any{"content": content, "encoding": "utf-8"})
	require.Equal(t, http.StatusCreated, status, out)

	return out["sha"].(string)
}

func (f *fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

func (f *fixture) cloneURL(user, token, repo string) string {
	parsed, _ := url.Parse(fakegithub.GitURL(f.api.URL, "acme", repo))
	parsed.User = url.UserPassword(user, token)

	return parsed.String()
}

func TestCommitSigningRule(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)
	blob := fix.blob(t, token, "hello\n")

	status, tree := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{
		"tree": []map[string]any{{"path": "a.txt", "mode": "100644", "type": "blob", "sha": blob}},
	})
	require.Equal(t, http.StatusCreated, status, tree)

	tests := map[string]struct {
		body         map[string]any
		wantVerified bool
		wantReason   string
		wantAuthor   string
	}{
		"no author: verified, authored by the App": {
			body:         map[string]any{"message": "m", "tree": tree["sha"]},
			wantVerified: true, wantReason: "valid", wantAuthor: "app[bot]",
		},
		"an author field: unsigned": {
			body: map[string]any{"message": "m", "tree": tree["sha"],
				"author": map[string]any{"name": "Jo", "email": "jo@example.com"}},
			wantReason: "unsigned", wantAuthor: "Jo",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			status, commit := fix.post(t, token, "/repos/acme/one/git/commits", test.body)
			require.Equal(t, http.StatusCreated, status, commit)

			verification := commit["verification"].(map[string]any)
			assert.Equal(t, test.wantVerified, verification["verified"])
			assert.Equal(t, test.wantReason, verification["reason"])
			assert.Equal(t, test.wantAuthor, commit["author"].(map[string]any)["name"])

			status, again := fix.do(t, http.MethodGet, "/repos/acme/one/git/commits/"+commit["sha"].(string), token, nil)
			require.Equal(t, http.StatusOK, status)
			assert.Equal(t, verification["verified"], again["verification"].(map[string]any)["verified"])
		})
	}

	t.Run("an unknown tree is refused", func(t *testing.T) {
		status, _ := fix.post(t, token, "/repos/acme/one/git/commits", map[string]any{"message": "m", "tree": strings.Repeat("a", 40)})
		assert.Equal(t, http.StatusUnprocessableEntity, status)
	})
}

func TestBlobRoundTrip(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)
	raw := []byte{0, 1, 2, '\n', '\n'}

	status, created := fix.post(t, token, "/repos/acme/one/git/blobs", map[string]any{"content": base64.StdEncoding.EncodeToString(raw), "encoding": "base64"})
	require.Equal(t, http.StatusCreated, status, created)

	status, got := fix.do(t, http.MethodGet, "/repos/acme/one/git/blobs/"+created["sha"].(string), token, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "base64", got["encoding"])
	assert.EqualValues(t, len(raw), got["size"])

	decoded, err := base64.StdEncoding.DecodeString(got["content"].(string))
	require.NoError(t, err)
	assert.Equal(t, raw, decoded)

	status, _ = fix.do(t, http.MethodGet, "/repos/acme/one/git/blobs/"+strings.Repeat("b", 40), token, nil)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestTreeBaseTreeDeletionsAndModes(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)
	one, two := fix.blob(t, token, "one\n"), fix.blob(t, token, "two\n")

	entry := func(path, mode string, sha any) map[string]any {
		return map[string]any{"path": path, "mode": mode, "type": "blob", "sha": sha}
	}

	status, base := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{"tree": []map[string]any{
		entry("keep.txt", "100644", one), entry("gone.txt", "100644", one), entry("dir/run.sh", "100755", one), entry("link", "120000", one),
	}})
	require.Equal(t, http.StatusCreated, status, base)

	status, next := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{"base_tree": base["sha"], "tree": []map[string]any{
		entry("gone.txt", "100644", nil), entry("new.txt", "100644", two),
	}})
	require.Equal(t, http.StatusCreated, status, next)

	status, commit := fix.post(t, token, "/repos/acme/one/git/commits", map[string]any{"message": "m", "tree": next["sha"]})
	require.Equal(t, http.StatusCreated, status, commit)

	status, ref := fix.post(t, token, "/repos/acme/one/git/refs", map[string]any{"ref": "refs/heads/main", "sha": commit["sha"]})
	require.Equal(t, http.StatusCreated, status, ref)

	dir := filepath.Join(t.TempDir(), "clone")
	fix.git(t, t.TempDir(), "clone", fix.cloneURL("x-access-token", token, "one"), dir)

	listing := fix.git(t, dir, "ls-tree", "-r", "HEAD")
	assert.Contains(t, listing, "100644 blob "+one+"\tkeep.txt")
	assert.Contains(t, listing, "100755 blob "+one+"\tdir/run.sh")
	assert.Contains(t, listing, "120000 blob "+one+"\tlink")
	assert.Contains(t, listing, "100644 blob "+two+"\tnew.txt")
	assert.NotContains(t, listing, "gone.txt")

	t.Run("invalid mode", func(t *testing.T) {
		status, _ := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{"tree": []map[string]any{entry("x", "100600", one)}})
		assert.Equal(t, http.StatusUnprocessableEntity, status)
	})
	t.Run("unknown base tree", func(t *testing.T) {
		status, _ := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{"base_tree": strings.Repeat("c", 40), "tree": []map[string]any{}})
		assert.Equal(t, http.StatusUnprocessableEntity, status)
	})
}

func TestGitRefsCreateAndUpdate(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)

	commit := func(parents ...any) string {
		status, tree := fix.post(t, token, "/repos/acme/one/git/trees", map[string]any{"tree": []map[string]any{
			{"path": "f", "mode": "100644", "type": "blob", "content": strings.Repeat("x", len(parents)+1)},
		}})
		require.Equal(t, http.StatusCreated, status, tree)

		status, out := fix.post(t, token, "/repos/acme/one/git/commits", map[string]any{"message": "m", "tree": tree["sha"], "parents": parents})
		require.Equal(t, http.StatusCreated, status, out)

		return out["sha"].(string)
	}

	first := commit()
	second := commit(first)
	unrelated := commit()

	status, _ := fix.post(t, token, "/repos/acme/one/git/refs", map[string]any{"ref": "refs/heads/work", "sha": first})
	require.Equal(t, http.StatusCreated, status)

	patch := func(ref, sha string, force bool) (int, map[string]any) {
		return fix.do(t, http.MethodPatch, "/repos/acme/one/git/refs/"+ref, token, map[string]any{"sha": sha, "force": force})
	}

	status, _ = fix.post(t, token, "/repos/acme/one/git/refs", map[string]any{"ref": "refs/heads/work", "sha": second})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "creating an existing ref")

	status, _ = fix.post(t, token, "/repos/acme/one/git/refs", map[string]any{"ref": "heads/bad", "sha": first})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "a ref outside refs/")

	status, out := patch("heads/work", second, false)
	assert.Equal(t, http.StatusOK, status, "fast forward")
	assert.Equal(t, second, out["object"].(map[string]any)["sha"])

	status, _ = patch("heads/work", unrelated, false)
	assert.Equal(t, http.StatusUnprocessableEntity, status, "not a fast forward")

	status, _ = patch("heads/work", unrelated, true)
	assert.Equal(t, http.StatusOK, status, "forced")

	status, _ = patch("heads/missing", first, true)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestGitBasicAuthAcceptsOnlyAccessTokenUser(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)

	for user, want := range map[string]int{"x-access-token": http.StatusOK, "octocat": http.StatusUnauthorized, "": http.StatusUnauthorized} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fakegithub.GitURL(fix.api.URL, "acme", "one")+"/info/refs?service=git-upload-pack", nil)
		require.NoError(t, err)
		request.SetBasicAuth(user, token)

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		_ = response.Body.Close()

		assert.Equal(t, want, response.StatusCode, "user %q", user)
	}
}

func TestReceivePackAdvertisesReportStatusAndPushWorks(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fakegithub.GitURL(fix.api.URL, "acme", "one")+"/info/refs?service=git-receive-pack", nil)
	require.NoError(t, err)
	request.SetBasicAuth("x-access-token", token)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	advert, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	_ = response.Body.Close()

	for _, capability := range []string{"report-status", "report-status-v2", "side-band-64k", "delete-refs"} {
		assert.Contains(t, string(advert), capability)
	}

	work := t.TempDir()
	fix.git(t, work, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o600))
	fix.git(t, work, "add", "f")
	fix.git(t, work, "commit", "-m", "first")
	fix.git(t, work, "push", fix.cloneURL("x-access-token", token, "one"), "main:refs/heads/agent/x")

	status, ref := fix.do(t, http.MethodGet, "/repos/acme/one/git/ref/heads/agent/x", token, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, fix.git(t, work, "rev-parse", "HEAD"), ref["object"].(map[string]any)["sha"])
}

func TestReleaseAssetUploadAndArchiveRedirect(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)

	status, release := fix.post(t, token, "/repos/acme/one/releases", map[string]any{"tag_name": "v1"})
	require.Equal(t, http.StatusCreated, status, release)

	status, _ = fix.post(t, token, "/repos/acme/one/releases", map[string]any{"tag_name": "v1"})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "a second release for a tag")

	assetPath := "/repos/acme/one/releases/" + strings.TrimSuffix(strings.TrimRight(release["upload_url"].(string)[strings.Index(release["upload_url"].(string), "/repos/")+len("/repos/acme/one/releases/"):], "}"), "{?name,label")
	upload := func(name string) int {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fix.api.URL+assetPath+"?name="+name, strings.NewReader("data"))
		require.NoError(t, err)
		request.Header.Set("Authorization", "token "+token)

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		_ = response.Body.Close()

		return response.StatusCode
	}

	assert.Equal(t, http.StatusCreated, upload("a.bin"))
	assert.Equal(t, http.StatusUnprocessableEntity, upload("a.bin"), "duplicate asset name")
	assert.Equal(t, http.StatusUnprocessableEntity, upload(""), "missing name")

	// An archive needs a commit on the branch.
	work := t.TempDir()
	fix.git(t, work, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o600))
	fix.git(t, work, "add", "f")
	fix.git(t, work, "commit", "-m", "first")
	fix.git(t, work, "push", fix.cloneURL("x-access-token", token, "one"), "main")

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fix.api.URL+"/repos/acme/one/tarball/main", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := noFollow.Do(request)
	require.NoError(t, err)

	_ = response.Body.Close()
	require.Equal(t, http.StatusFound, response.StatusCode)

	location := response.Header.Get("Location")
	assert.NotContains(t, location, token, "the redirect must not carry the installation token")

	// The storage URL needs no credential.
	archive, err := http.NewRequestWithContext(t.Context(), http.MethodGet, location, nil)
	require.NoError(t, err)

	got, err := http.DefaultClient.Do(archive)
	require.NoError(t, err)

	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()

	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Contains(t, string(body), "f")
}

func TestGraphQLCreatePullRequest(t *testing.T) {
	fix := newFixture(t)
	require.NoError(t, fix.server.CreateInstallation(fakegithub.Installation{
		ID: 71, AppID: 7, Owner: "acme", Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
	}))

	_, minted := fix.do(t, http.MethodPost, "/app/installations/71/access_tokens", fix.goodJWT(t), nil)
	withPulls := minted["token"].(string)
	noPulls := fix.token(t, nil)

	status, tree := fix.post(t, withPulls, "/repos/acme/one/git/trees", map[string]any{"tree": []map[string]any{{"path": "f", "mode": "100644", "type": "blob", "content": "x"}}})
	require.Equal(t, http.StatusCreated, status)

	_, commit := fix.post(t, withPulls, "/repos/acme/one/git/commits", map[string]any{"message": "m", "tree": tree["sha"]})
	for _, branch := range []string{"main", "feature"} {
		status, _ = fix.post(t, withPulls, "/repos/acme/one/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": commit["sha"]})
		require.Equal(t, http.StatusCreated, status)
	}

	_, repo := fix.post(t, withPulls, "/graphql", map[string]any{"query": `query { repository(owner: "acme", name: "one") { id nameWithOwner } }`})
	id := repo["data"].(map[string]any)["repository"].(map[string]any)["id"]

	mutation := map[string]any{
		"query":     `mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { number url } } }`,
		"variables": map[string]any{"input": map[string]any{"repositoryId": id, "baseRefName": "main", "headRefName": "feature", "title": "t"}},
	}

	_, denied := fix.post(t, noPulls, "/graphql", mutation)
	assert.Contains(t, denied["errors"].([]any)[0].(map[string]any)["type"], "FORBIDDEN")
	assert.Empty(t, fix.server.PullRequests("acme", "one"))

	status, created := fix.post(t, withPulls, "/graphql", mutation)
	require.Equal(t, http.StatusOK, status)
	require.Nil(t, created["errors"], created)

	pull := created["data"].(map[string]any)["createPullRequest"].(map[string]any)["pullRequest"].(map[string]any)
	assert.EqualValues(t, 1, pull["number"])
	assert.Len(t, fix.server.PullRequests("acme", "one"), 1)

	_, broken := fix.post(t, withPulls, "/graphql", map[string]any{"query": "mutation {"})
	assert.NotEmpty(t, broken["errors"])
}

func TestServesTheThreeGitHubHostsWithTheTestCA(t *testing.T) {
	fix := newFixture(t)
	token := fix.token(t, nil)

	authority, err := fakegithub.NewTestCA()
	require.NoError(t, err)

	config, err := authority.TLSConfig()
	require.NoError(t, err)

	server := fakegithubTLS(t, fix, config)

	// A pod's hostAliases map each name to the fake's address; the client dials
	// that address whatever the name.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: authority.Pool(), MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context_, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", server)
		},
	}}

	for _, host := range fakegithub.GitHubHosts {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/rate_limit", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+token)

		response, err := client.Do(request)
		require.NoError(t, err, host)

		_ = response.Body.Close()
		assert.Equal(t, http.StatusOK, response.StatusCode, host)
	}

	_, err = client.Get("https://example.com/rate_limit") //nolint:noctx // fails at the handshake
	require.Error(t, err, "a name outside the three hosts must fail the TLS handshake")
}

type context_ = context.Context

func fakegithubTLS(t *testing.T, fix *fixture, config *tls.Config) string {
	t.Helper()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	require.NoError(t, err)

	server := &http.Server{Handler: fix.server.Handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(func() { _ = server.Close() })

	return listener.Addr().String()
}
