//go:build live

package live_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	foreignCredential = "ghp_foreignCredentialThatMustNeverReachGitHub0000"
	workloadWork      = "/work"
	workloadTrust     = "/trust"
)

// workload is a container that runs the interceptor and the tools of a pod.
// The three GitHub names resolve to the container itself, where the
// interceptor listens on port 443 and forwards to the in-process proxy.
type workload struct {
	name string
	live *liveEnv
}

// authorizations remembers every Authorization header the proxy receives.
type authorizations struct {
	mu     sync.Mutex
	values []string
}

func (a *authorizations) observe(request *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.values = append(a.values, request.Header.Values("Authorization")...)
}

func (a *authorizations) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.values...)
}

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "docker", args...)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// startWorkload builds the workload image, starts a container for account and
// starts the interceptor in it. It returns once the interceptor's probe passes.
func (l *liveEnv) startWorkload(t *testing.T, account string, proxyListener string, proxyCA string) *workload {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed; skipping")
	}

	// A static Linux client for the container, whatever the host is.
	work := filepath.Join(l.tmp, "workload")
	require.NoError(t, os.MkdirAll(work, 0o755))

	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(work, clientBinary), "github.com/solidDoWant/githubgate/cmd/githubgate-client")

	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	token, err := os.ReadFile(l.tokenFile(account))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(work, "token"), token, 0o644))

	caPEM, err := os.ReadFile(proxyCA)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(work, "proxy-ca.crt"), caPEM, 0o644))

	dockerfile, err := filepath.Abs("Dockerfile.workload")
	require.NoError(t, err)

	image := "githubgate-live-workload:" + unique(t)
	text, err := docker(t, "build", "--quiet", "-t", image, "-f", dockerfile, l.tmp)
	require.NoError(t, err, text)

	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", image).Run() })

	name := "githubgate-live-" + unique(t)
	text, err = docker(t, "run", "-d", "--name", name,
		"--add-host", "api.github.com:127.0.0.1",
		"--add-host", "uploads.github.com:127.0.0.1",
		"--add-host", "github.com:127.0.0.1",
		"--add-host", "githubgate-proxy.invalid:host-gateway",
		"-v", work+":"+workloadWork+":ro",
		image, "sleep", "infinity")
	require.NoError(t, err, text)

	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	_, port, ok := strings.Cut(proxyListener, ":")
	require.True(t, ok)

	text, err = docker(t, "exec", "-d", name, workloadWork+"/"+clientBinary, "intercept",
		"--listen", "127.0.0.1:443",
		"--proxy-address", "githubgate-proxy.invalid:"+port,
		"--proxy-ca", "@"+workloadWork+"/proxy-ca.crt",
		"--token-file", workloadWork+"/token",
		"--trust-dir", workloadTrust,
		"--state-dir", "/state")
	require.NoError(t, err, text)

	w := &workload{name: name, live: l}

	require.Eventually(t, func() bool {
		_, err := docker(t, "exec", name, workloadWork+"/"+clientBinary, "intercept", "--probe")

		return err == nil
	}, time.Minute, time.Second, "the interceptor never became ready")

	return w
}

// run runs a command in the container as a pod's workload would: with the
// trust bundle and a placeholder GitHub token, and nothing else set up.
func (w *workload) run(t *testing.T, dir, name string, args ...string) (string, error) {
	t.Helper()

	bundle := workloadTrust + "/bundle.crt"
	full := append([]string{
		"exec", "-w", dir,
		"-e", "HOME=/tmp/home",
		"-e", "SSL_CERT_FILE=" + bundle, "-e", "GIT_SSL_CAINFO=" + bundle, "-e", "CURL_CA_BUNDLE=" + bundle,
		"-e", "GH_TOKEN=githubgate", "-e", "GH_PROMPT_DISABLED=1", "-e", "GIT_TERMINAL_PROMPT=0",
		"-e", "GIT_AUTHOR_NAME=githubgate live test", "-e", "GIT_AUTHOR_EMAIL=live-test@example.invalid",
		"-e", "GIT_COMMITTER_NAME=githubgate live test", "-e", "GIT_COMMITTER_EMAIL=live-test@example.invalid",
		w.name, name,
	}, args...)

	out, err := docker(t, full...)
	w.live.log.printf(t, "$ (container) %s %s\n%s", name, strings.Join(args, " "), out)

	return out, err
}

func TestLiveInterceptionAgainstGitHub(t *testing.T) {
	live := setup(t)
	seen := &authorizations{}
	listener, caFile := live.serveProxy(t, "0.0.0.0:0", seen.observe)
	slug := live.owner + "/" + live.repo
	cloneURL := "https://github.com/" + slug + ".git"

	box := live.startWorkload(t, pushAccount, listener.Addr().String(), caFile)

	_, err := box.run(t, "/tmp", "mkdir", "-p", "/tmp/home")
	require.NoError(t, err)

	t.Run("gh api reads an allowed repository with no setup", func(t *testing.T) {
		out, err := box.run(t, "/tmp", "gh", "api", "repos/"+slug, "--jq", ".name")
		require.NoError(t, err)
		assert.Equal(t, live.repo, strings.TrimSpace(out))
	})

	t.Run("git clone, fetch and a push to an allowed ref work and another ref is refused", func(t *testing.T) {
		_, err := box.run(t, "/tmp", "git", "clone", cloneURL, "/tmp/work")
		require.NoError(t, err)

		_, err = box.run(t, "/tmp/work", "git", "fetch", "origin")
		require.NoError(t, err)

		name := unique(t)
		_, err = box.run(t, "/tmp/work", "git", "checkout", "-b", name)
		require.NoError(t, err)
		_, err = box.run(t, "/tmp/work", "sh", "-c", "echo githubgate live test > "+name+".txt")
		require.NoError(t, err)
		_, err = box.run(t, "/tmp/work", "git", "add", name+".txt")
		require.NoError(t, err)
		_, err = box.run(t, "/tmp/work", "git", "commit", "-m", "githubgate live test")
		require.NoError(t, err)

		allowed := "agent/" + name

		t.Cleanup(func() { live.deleteBranch(t, allowed) })

		_, err = box.run(t, "/tmp/work", "git", "push", "origin", "HEAD:refs/heads/"+allowed)
		require.NoError(t, err)

		out, err := box.run(t, "/tmp/work", "git", "push", "origin", "HEAD:refs/heads/"+name)
		require.Error(t, err)
		assert.Contains(t, out, "refs/heads/"+name)
	})

	t.Run("a foreign credential is replaced before the proxy and never reaches GitHub", func(t *testing.T) {
		out, err := box.run(t, "/tmp", "curl", "-sS", "-H", "Authorization: Bearer "+foreignCredential,
			"-H", "Cookie: session="+foreignCredential, "https://api.github.com/repos/"+slug)
		require.NoError(t, err)
		assert.Contains(t, out, `"name"`, "the request ran as the workload's ServiceAccount, not as the foreign credential")

		out, err = box.run(t, "/tmp", "gh", "api", "repos/"+slug, "--jq", ".name",
			"-H", "Authorization: token "+foreignCredential)
		require.NoError(t, err)
		assert.Equal(t, live.repo, strings.TrimSpace(out))

		for _, value := range seen.all() {
			assert.NotContains(t, value, foreignCredential, "the proxy received the foreign credential")
		}

		assert.NotEmpty(t, seen.all(), "the proxy saw the requests")
	})

	live.assertNoSecretsInTranscript(t)
}
