package proxy_test

import (
	"crypto/rand"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// gitThroughProxy runs a git http-backend upstream and the proxy in front of
// it, and returns the proxy's URL (Host rewritten to github.com).
func gitThroughProxy(t *testing.T, rules ...*model.Rule) (string, string, *[]string) {
	t.Helper()

	root := t.TempDir()
	run(t, root, "git", "init", "--bare", "-q", "acme/foo.git")

	execPath, err := exec.Command("git", "--exec-path").Output()
	require.NoError(t, err)

	var mu sync.Mutex
	seen := []string{}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Dir:  root,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"},
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen = append(seen, request.Method+" "+request.URL.Path)
		mu.Unlock()
		backend.ServeHTTP(writer, request)
	}))
	t.Cleanup(upstream.Close)

	base, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:     func(string, string) []*model.Rule { return rules },
		Tokens:    &fakeTokens{},
		Upstreams: proxy.Upstreams{API: base, Uploads: base, Git: base},
		Transport: upstream.Client().Transport,
	})
	require.NoError(t, err)

	handler := proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder})
	front := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		request.Host = proxy.HostGit
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(front.Close)

	return front.URL + "/acme/foo.git", root, &seen
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %v: %s", name, args, out)

	return string(out)
}

func TestBughuntGitPushThroughProxy(t *testing.T) {
	for name, size := range map[string]int{"small push": 1 << 10, "push larger than http.postBuffer": 4 << 20} {
		t.Run(name, func(t *testing.T) {
			remote, _, seen := gitThroughProxy(t, agentRule())
			work := t.TempDir()
			run(t, work, "git", "init", "-q", "-b", "main")
			blob := make([]byte, size)
			_, _ = rand.Read(blob)
			require.NoError(t, os.WriteFile(filepath.Join(work, "data.bin"), blob, 0o600))
			run(t, work, "git", "add", "data.bin")
			run(t, work, "git", "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "-m", "x")

			cmd := exec.CommandContext(t.Context(), "git", "-c", "http.sslVerify=false",
				"-c", "http.extraHeader=Authorization: Bearer "+validToken,
				"push", remote, "HEAD:refs/heads/agent/x")
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
			out, err := cmd.CombinedOutput()
			t.Logf("upstream saw: %v", *seen)
			require.NoError(t, err, "%s", out)
		})
	}
}

func TestBughuntGitCloneManyRefsThroughProxy(t *testing.T) {
	for name, branches := range map[string]int{"5 branches": 5, "60 branches": 60} {
		t.Run(name, func(t *testing.T) {
			remote, root, seen := gitThroughProxy(t, agentRule())
			seed := t.TempDir()
			run(t, seed, "git", "init", "-q", "-b", "main")
			run(t, seed, "git", "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "--allow-empty", "-m", "base")
			for index := range branches {
				run(t, seed, "git", "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "--allow-empty", "-m", "c")
				run(t, seed, "git", "branch", "b"+strconv.Itoa(index))
			}
			run(t, seed, "git", "push", "-q", filepath.Join(root, "acme/foo.git"), "refs/heads/*:refs/heads/*")

			cmd := exec.CommandContext(t.Context(), "git", "-c", "http.sslVerify=false",
				"-c", "http.extraHeader=Authorization: Bearer "+validToken,
				"clone", "-q", remote, filepath.Join(t.TempDir(), "clone"))
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
			out, err := cmd.CombinedOutput()
			t.Logf("upstream saw: %v", *seen)
			require.NoError(t, err, "%s", out)
		})
	}
}
