package proxy_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// gitRemoteThroughProxy serves a bare repository acme/foo with git
// http-backend and puts the proxy in front of it. It returns the proxy's URL
// of the repository and the bare repository's path.
func gitRemoteThroughProxy(t *testing.T, rules ...*model.Rule) (string, string) {
	t.Helper()

	root := t.TempDir()
	bare := filepath.Join(root, "acme", "foo.git")
	runGit(t, root, "init", "--bare", "-q", bare)

	execPath := strings.TrimSpace(runGit(t, root, "--exec-path"))
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Dir:  root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
		},
	}
	// GitHub takes a chunked push, but Go's CGI handler doesn't: the body
	// is read first so the backend gets its length.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		request.TransferEncoding = nil
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

	return front.URL + "/acme/foo.git", bare
}

func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()

	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = dir

	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")

	return command
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	output, err := gitCommand(t, dir, args...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, output)

	return string(output)
}

// commitOfSize makes a repository with one commit adding size random bytes,
// which don't compress.
func commitOfSize(t *testing.T, size int) string {
	t.Helper()

	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")

	blob := make([]byte, size)
	_, err := rand.Read(blob)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(work, "data.bin"), blob, 0o600))

	runGit(t, work, "add", "data.bin")
	runGit(t, work, "-c", "user.name=a", "-c", "user.email=a@example.com", "commit", "-q", "-m", "data")

	return work
}

func TestGitPushThroughProxy(t *testing.T) {
	// git sends a probe before a push larger than http.postBuffer (1 MiB).
	tests := []struct {
		name          string
		size          int
		ref           string
		expectedError require.ErrorAssertionFunc
	}{
		{name: "a small push to an allowed ref", size: 1 << 10, ref: "refs/heads/agent/x"},
		{name: "a push larger than git's post buffer to an allowed ref", size: 4 << 20, ref: "refs/heads/agent/x"},
		{name: "a push larger than git's post buffer outside pushRefs", size: 4 << 20, ref: "refs/heads/main", expectedError: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.expectedError == nil {
				test.expectedError = require.NoError
			}

			remote, bare := gitRemoteThroughProxy(t, agentRule())
			work := commitOfSize(t, test.size)

			push := gitCommand(t, work, "-c", "http.sslVerify=false",
				"-c", "http.extraHeader=Authorization: Bearer "+validToken,
				"push", remote, "HEAD:"+test.ref)
			output, err := push.CombinedOutput()
			test.expectedError(t, err, "%s", output)

			refs := runGit(t, bare, "for-each-ref", "--format=%(refname)")
			if err != nil {
				assert.Empty(t, strings.TrimSpace(refs), "a refused push wrote a ref")
			} else {
				assert.Equal(t, test.ref, strings.TrimSpace(refs))
			}
		})
	}
}
