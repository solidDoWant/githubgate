package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

// pushFixture is a local clone whose github.com remote is redirected to a bare
// repository of the fake GitHub, plus the token API and the fake's API.
type pushFixture struct {
	t         *testing.T
	server    *fakegithub.Server
	bare      string
	work      string
	vars      map[string]string
	api       *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	requests  []recordedRequest
	intercept func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()

	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Ann Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "ann@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Ann Author")
	t.Setenv("GIT_COMMITTER_EMAIL", "ann@example.com")

	fix := &pushFixture{t: t}

	var err error

	root := t.TempDir()
	fix.server, err = fakegithub.New(root)
	require.NoError(t, err)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	fix.key = key

	fix.server.CreateApp(fakegithub.App{ID: 7, Name: "app", PublicKey: &key.PublicKey})
	require.NoError(t, fix.server.CreateInstallation(fakegithub.Installation{
		ID: 70, AppID: 7, Owner: "acme", Permissions: map[string]string{"contents": "write"},
	}))
	require.NoError(t, fix.server.CreateRepository("acme", "one", false))

	handler := fix.server.Handler()
	fix.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))

		var body map[string]any

		_ = json.Unmarshal(raw, &body)

		fix.mu.Lock()
		fix.requests = append(fix.requests, recordedRequest{r.Method, r.URL.Path, body})
		intercept := fix.intercept
		fix.mu.Unlock()

		if intercept != nil && intercept(w, r, raw) {
			return
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fix.api.Close)

	tokenAPI := newFakeAPI(t, func(_ int, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"token":"` + fix.mintToken(key) + `","expiresAt":"2030-01-01T00:00:00Z"}`))
	})

	fix.vars = tokenAPI.envelope
	fix.vars["GITHUBGATE_GITHUB_API_URL"] = fix.api.URL

	fix.bare = filepath.Join(root, "acme", "one.git")

	fix.work = t.TempDir()
	gitIn(t, fix.work, "init", "-q", "--initial-branch=main")
	gitIn(t, fix.work, "config", "user.name", "Ann Author")
	gitIn(t, fix.work, "config", "user.email", "ann@example.com")
	gitIn(t, fix.work, "remote", "add", "origin", "https://github.com/acme/one.git")
	gitIn(t, fix.work, "config", "url."+fix.bare+".insteadOf", "https://github.com/acme/one.git")

	fix.commit("base.txt", "base\n", "base")
	gitIn(t, fix.work, "push", "-q", "origin", "main")
	t.Chdir(fix.work)

	return fix
}

func (f *pushFixture) mintToken(key *rsa.PrivateKey) string {
	f.t.Helper()

	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "7", "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix()}).SignedString(key)
	require.NoError(f.t, err)

	request, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, f.api.URL+"/app/installations/70/access_tokens", strings.NewReader(`{}`))
	require.NoError(f.t, err)

	request.Header.Set("Authorization", "Bearer "+signed)

	response, err := http.DefaultClient.Do(request)
	require.NoError(f.t, err)

	defer func() { _ = response.Body.Close() }()

	var out struct {
		Token string `json:"token"`
	}

	require.NoError(f.t, json.NewDecoder(response.Body).Decode(&out))

	return out.Token
}

func (f *pushFixture) commit(path, content, message string) {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(filepath.Join(f.work, path)), 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(f.work, path), []byte(content), 0o644))
	gitIn(f.t, f.work, "add", path)
	gitIn(f.t, f.work, "commit", "-q", "-m", message)
}

func (f *pushFixture) push(args ...string) (string, error) {
	f.t.Helper()

	out, _, err := runClient(f.t, f.vars, "", append([]string{"push"}, args...)...)

	return out, err
}

func (f *pushFixture) remoteTip(branch string) string {
	f.t.Helper()

	return gitIn(f.t, f.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
}

func TestPushPublishesCommitsThroughTheAPI(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "-b", "feature")

	fix.commit("copy.txt", "base\n", "copy of an existing blob")
	fix.commit("new.txt", "new content\n", "add a file")
	require.NoError(t, os.WriteFile(filepath.Join(fix.work, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink("new.txt", filepath.Join(fix.work, "link")))
	gitIn(t, fix.work, "add", "run.sh", "link")
	gitIn(t, fix.work, "commit", "-q", "-m", "executable and symlink\n\nbody")
	gitIn(t, fix.work, "rm", "-q", "base.txt")
	gitIn(t, fix.work, "update-index", "--chmod=+x", "new.txt")
	gitIn(t, fix.work, "commit", "-q", "-m", "delete and chmod")

	before := gitIn(t, fix.work, "rev-parse", "HEAD")
	beforeTree := gitIn(t, fix.work, "rev-parse", "HEAD^{tree}")

	out, err := fix.push("origin", "feature")
	require.NoError(t, err)
	assert.Contains(t, out, "published 4 commit(s)")

	tip := fix.remoteTip("feature")
	assert.NotEqual(t, before, tip, "the published commits are new commits")
	assert.Equal(t, beforeTree, gitIn(t, fix.bare, "rev-parse", tip+"^{tree}"), "same files, modes and deletions")
	assert.Equal(t, 4, strings.Count(gitIn(t, fix.bare, "rev-list", "main.."+tip), "\n")+1)

	listing := gitIn(t, fix.bare, "ls-tree", "-r", tip)
	assert.Contains(t, listing, "100755 blob")
	assert.Contains(t, listing, "120000 blob")
	assert.NotContains(t, listing, "base.txt")

	assert.Equal(t, "app[bot]", gitIn(t, fix.bare, "log", "-1", "--format=%an", tip))
	assert.Contains(t, gitIn(t, fix.bare, "log", "-1", "--format=%B", tip), "Co-authored-by: Ann Author <ann@example.com>")
	assert.Contains(t, gitIn(t, fix.bare, "log", "-1", "--format=%B", tip+"~1"), "body")

	fix.mu.Lock()
	defer fix.mu.Unlock()

	var blobPosts, commitPosts int

	for _, request := range fix.requests {
		switch {
		case request.method == http.MethodPost && strings.HasSuffix(request.path, "/git/blobs"):
			blobPosts++
		case request.method == http.MethodPost && strings.HasSuffix(request.path, "/git/commits"):
			commitPosts++

			assert.NotContains(t, request.body, "author")
			assert.NotContains(t, request.body, "committer")
		case request.method == http.MethodPatch:
			assert.Equal(t, false, request.body["force"])
		}
	}

	assert.Equal(t, 4, commitPosts)
	assert.Equal(t, 3, blobPosts, "the copy of an existing blob is reused: new.txt, run.sh and the symlink target are created")
}

func TestPushMovesTheLocalBranchAndKeepsTheWorkingTree(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "-b", "feature")
	fix.commit("a.txt", "a\n", "add a")
	require.NoError(t, os.WriteFile(filepath.Join(fix.work, "a.txt"), []byte("uncommitted\n"), 0o644))

	_, err := fix.push()
	require.NoError(t, err)

	assert.Equal(t, fix.remoteTip("feature"), gitIn(t, fix.work, "rev-parse", "HEAD"))
	assert.Equal(t, "feature", gitIn(t, fix.work, "branch", "--show-current"))
	assert.Equal(t, "M a.txt", gitIn(t, fix.work, "status", "--porcelain"), "the index and working tree are unchanged")

	content, err := os.ReadFile(filepath.Join(fix.work, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted\n", string(content))
}

func TestPushPublishesOntoAnExistingRemoteBranch(t *testing.T) {
	fix := newPushFixture(t)
	fix.commit("b.txt", "b\n", "add b")

	_, err := fix.push("origin", "main")
	require.NoError(t, err)

	assert.Equal(t, fix.remoteTip("main"), gitIn(t, fix.work, "rev-parse", "HEAD"))

	fix.mu.Lock()
	defer fix.mu.Unlock()

	var moves int

	for _, request := range fix.requests {
		if request.method == http.MethodPatch {
			moves++

			assert.Equal(t, false, request.body["force"])
		}
	}

	assert.Equal(t, 1, moves, "the branch moves once")
	assert.Equal(t, 1, strings.Count(gitIn(t, fix.bare, "log", "--format=%H", "main"), "\n"), "base plus one commit")
}

func TestPushRefusesWhatTheAPICannotExpress(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, fix *pushFixture) string
		wantMsg string
	}{
		"a merge commit": {
			prepare: func(t *testing.T, fix *pushFixture) string {
				gitIn(t, fix.work, "checkout", "-q", "-b", "side")
				fix.commit("side.txt", "side\n", "side")
				gitIn(t, fix.work, "checkout", "-q", "main")
				fix.commit("main.txt", "main\n", "main")
				gitIn(t, fix.work, "merge", "-q", "--no-ff", "-m", "merge side", "side")

				return gitIn(t, fix.work, "rev-parse", "HEAD")
			},
			wantMsg: "merge commit",
		},
		"a submodule": {
			prepare: func(t *testing.T, fix *pushFixture) string {
				gitIn(t, fix.work, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",vendor/lib")
				gitIn(t, fix.work, "commit", "-q", "-m", "add submodule")

				return gitIn(t, fix.work, "rev-parse", "HEAD")
			},
			wantMsg: `submodule "vendor/lib"`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fix := newPushFixture(t)
			before := fix.remoteTip("main")
			bad := tt.prepare(t, fix)

			_, err := fix.push("origin", "main")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.NotEmpty(t, bad)

			if name == "a submodule" {
				assert.Contains(t, err.Error(), bad, "the message names the commit")
			}

			assert.Equal(t, before, fix.remoteTip("main"), "nothing is published")
			assert.Equal(t, bad, gitIn(t, fix.work, "rev-parse", "HEAD"))

			fix.mu.Lock()
			defer fix.mu.Unlock()

			for _, request := range fix.requests {
				assert.NotContains(t, request.path, "/git/", "no Git Database request is sent")
			}
		})
	}
}

func TestPushRefusesADetachedHead(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "--detach")

	_, err := fix.push()
	require.ErrorContains(t, err, "detached")
}

func TestPushPublishesNothingOnARateLimit(t *testing.T) {
	fix := newPushFixture(t)
	fix.commit("c.txt", "c\n", "add c")

	before := fix.remoteTip("main")

	fix.mu.Lock()
	fix.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/git/blobs") {
			return false
		}

		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation."}`))

		return true
	}
	fix.mu.Unlock()

	_, err := fix.push()
	require.ErrorContains(t, err, "API rate limit exceeded for installation.")
	assert.Equal(t, before, fix.remoteTip("main"))
}

func TestPushChangesNothingWhenTheRemoteBranchMoved(t *testing.T) {
	fix := newPushFixture(t)
	fix.commit("d.txt", "d\n", "add d")

	local := gitIn(t, fix.work, "rev-parse", "HEAD")

	fix.mu.Lock()
	fix.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodPatch {
			return false
		}

		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Update is not a fast forward"}`))

		return true
	}
	fix.mu.Unlock()

	_, err := fix.push()
	require.ErrorContains(t, err, "nothing was changed")
	assert.Equal(t, local, gitIn(t, fix.work, "rev-parse", "HEAD"), "the local branch stays")
}

func TestPushCreatesAMissingRemoteBranch(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "-b", "brand/new")
	fix.commit("e.txt", "e\n", "add e")

	_, err := fix.push()
	require.NoError(t, err)
	assert.Equal(t, fix.remoteTip("brand/new"), gitIn(t, fix.work, "rev-parse", "HEAD"))
}

func TestPushDoesNotRepeatAnExistingTrailer(t *testing.T) {
	fix := newPushFixture(t)
	fix.commit("f.txt", "f\n", "add f\n\nCo-authored-by: Ann Author <ann@example.com>")

	_, err := fix.push()
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(gitIn(t, fix.bare, "log", "-1", "--format=%B", fix.remoteTip("main")), "Co-authored-by"))
}

func TestPushLeavesTheLocalBranchWhenTheTreesDiffer(t *testing.T) {
	fix := newPushFixture(t)
	fix.commit("g.txt", "g\n", "add g")

	local := gitIn(t, fix.work, "rev-parse", "HEAD")

	fix.mu.Lock()
	fix.intercept = func(_ http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/blobs") {
			r.Body = io.NopCloser(strings.NewReader(`{"content":"tampered","encoding":"utf-8"}`))
			r.ContentLength = -1
		}

		return false
	}
	fix.mu.Unlock()

	out, err := fix.push()
	require.NoError(t, err)
	assert.Contains(t, out, "left alone")
	assert.Contains(t, out, gitIn(t, fix.work, "rev-parse", "HEAD^{tree}"))
	assert.Equal(t, local, gitIn(t, fix.work, "rev-parse", "HEAD"))
}
