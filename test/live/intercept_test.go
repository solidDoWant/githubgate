//go:build live

package live_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

	// dockerNetworkEnv, when set (for example container:<id>), is the network
	// the workload joins; the proxy is then reached on 127.0.0.1.
	dockerNetworkEnv = "GITHUBGATE_LIVE_DOCKER_NETWORK"
	interceptListen  = "127.43.0.1:443"
	proxyHostName    = "githubgate-proxy.invalid"
)

// workload is a container that runs the interceptor and the tools of a pod.
// The three GitHub names resolve to the container itself, where the
// interceptor listens on port 443 and forwards to the in-process proxy.
type workload struct {
	name    string
	live    *liveEnv
	network string

	mu   sync.Mutex
	runs int
}

// authorizations remembers every Authorization header the proxy receives,
// and the method, host and path of each request (never a header or query).
type authorizations struct {
	mu       sync.Mutex
	values   []string
	requests []string
}

func (a *authorizations) observe(request *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.values = append(a.values, request.Header.Values("Authorization")...)
	a.requests = append(a.requests, request.Method+" "+request.Host+" "+request.URL.Path+" "+request.Proto)
}

// received lists the requests the proxy received, oldest first.
func (a *authorizations) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.requests...)
}

func (a *authorizations) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.values...)
}

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()

	// Not cancelled with the test, so cleanups can still run docker.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", args...)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// dockerSplit runs docker with its standard output and standard error kept
// apart, so a tool's debug trace on stderr doesn't mix with its result.
func dockerSplit(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

// exitCode is the exit code err reports: 0 for nil, -1 when the command
// didn't run to an exit.
func exitCode(err error) int {
	if err == nil {
		return 0
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}

	return -1
}

// startWorkload builds the workload image, starts a container for account and
// starts the interceptor in it. It returns once the interceptor's probe passes.
func (l *liveEnv) startWorkload(t *testing.T, account string, proxyListener string, proxyCA string, network string) *workload {
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
	text, err = docker(t, workloadRunArgs(name, image, network)...)
	require.NoError(t, err, text)

	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	// Nothing is bind mounted: the daemon may not see this machine's paths.
	text, err = docker(t, "exec", name, "mkdir", "-p", workloadWork, workloadRuns)
	require.NoError(t, err, text)

	for _, file := range []string{clientBinary, "token", "proxy-ca.crt"} {
		text, err = docker(t, "cp", filepath.Join(work, file), name+":"+workloadWork+"/"+file)
		require.NoError(t, err, text)
	}

	// --add-host would resolve on the daemon's side too; the names are set up in the container.
	text, err = docker(t, "exec", name, "sh", "-c", hostsSetupCommand(network))
	require.NoError(t, err, text)

	_, port, ok := strings.Cut(proxyListener, ":")
	require.True(t, ok)

	text, err = docker(t, interceptExecArgs(name, network, port)...)
	require.NoError(t, err, text)

	w := &workload{name: name, live: l, network: network}

	ready := assert.Eventually(t, func() bool {
		_, err := docker(t, interceptProbeArgs(name, network)...)

		return err == nil
	}, time.Minute, time.Second, "the interceptor never became ready")
	if !ready {
		output, _ := docker(t, "exec", name, "cat", interceptLogFile)
		t.Logf("interceptor output:\n%s", output)
		t.FailNow()
	}

	return w
}

// run runs a command in the container as a pod's workload would: with the
// trust bundle and a placeholder GitHub token, and nothing else set up.
func (w *workload) run(t *testing.T, dir, name string, args ...string) (string, error) {
	t.Helper()

	return w.runWithEnv(t, nil, dir, name, args...)
}

// runWithEnv is run with extra variables, each NAME=value. It returns the
// command's standard output followed by its standard error.
func (w *workload) runWithEnv(t *testing.T, env []string, dir, name string, args ...string) (string, error) {
	t.Helper()

	stdout, stderr, err := w.runSplitOutput(t, env, dir, name, args...)

	return stdout + stderr, err
}

// runSplit is runWithEnv with only the command's standard output returned.
func (w *workload) runSplit(t *testing.T, env []string, dir, name string, args ...string) (string, error) {
	t.Helper()

	stdout, _, err := w.runSplitOutput(t, env, dir, name, args...)

	return stdout, err
}

// runSplitOutput runs a command in the container and returns what the
// command itself wrote and how it exited, as recorded inside the container.
// docker exec can return before the command ends (it then reports exit code
// 0 and whatever output it had copied so far), so the result is read from
// files the command's shell writes when the command has really finished.
func (w *workload) runSplitOutput(t *testing.T, env []string, dir, name string, args ...string) (string, string, error) {
	t.Helper()

	w.mu.Lock()
	w.runs++
	base := fmt.Sprintf("%s/%d", workloadRuns, w.runs)
	w.mu.Unlock()

	recorded := recordedCommand(base, append([]string{name}, args...)...)

	execOut, execErr, err := dockerSplit(t, w.execArgs(env, dir, recorded[0], recorded[1:]...)...)
	if err != nil || execOut != "" || execErr != "" {
		w.live.log.printf(t, "docker exec (exit code %d, err %v) printed:\n%s%s", exitCode(err), err, execOut, execErr)
	}

	code, finished := w.recordedExitCode(t, base)
	if !finished {
		w.live.log.printf(t, "docker exec returned (exit code %d) before %s ended; waiting for it", exitCode(err), name)

		require.Eventually(t, func() bool {
			code, finished = w.recordedExitCode(t, base)

			return finished
		}, 5*time.Minute, 500*time.Millisecond, "%s never ended in the container", name)
	}

	stdout, _ := docker(t, "exec", w.name, "cat", base+".out")
	stderr, _ := docker(t, "exec", w.name, "cat", base+".err")

	w.live.log.printf(t, "$ (container) %s %s %s\nexit code %d\nstdout (%d bytes):\n%s\nstderr:\n%s",
		strings.Join(env, " "), name, strings.Join(args, " "), code, len(stdout), stdout, stderr)

	if code != 0 {
		return stdout, stderr, fmt.Errorf("%s exited with code %d", name, code)
	}

	return stdout, stderr, nil
}

// recordedExitCode reads the exit code recordedCommand wrote for base, and
// whether the command has ended.
func (w *workload) recordedExitCode(t *testing.T, base string) (int, bool) {
	t.Helper()

	text, err := docker(t, "exec", w.name, "cat", base+".rc")
	if err != nil {
		return 0, false
	}

	code, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}

	return code, true
}

// execArgs are the docker arguments that run a command in the container as
// a pod's workload would.
func (w *workload) execArgs(env []string, dir, name string, args ...string) []string {
	bundle := workloadTrust + "/bundle.crt"

	full := []string{"exec", "-w", dir}
	for _, variable := range env {
		full = append(full, "-e", variable)
	}

	full = append(full, []string{
		"-e", "HOME=/tmp/home",
		"-e", "SSL_CERT_FILE=" + bundle, "-e", "GIT_SSL_CAINFO=" + bundle, "-e", "CURL_CA_BUNDLE=" + bundle,
		"-e", "GH_TOKEN=githubgate", "-e", "GH_PROMPT_DISABLED=1", "-e", "GIT_TERMINAL_PROMPT=0",
		"-e", "GIT_AUTHOR_NAME=githubgate live test", "-e", "GIT_AUTHOR_EMAIL=live-test@example.invalid",
		"-e", "GIT_COMMITTER_NAME=githubgate live test", "-e", "GIT_COMMITTER_EMAIL=live-test@example.invalid",
		w.name,
	}...)

	return append(full, inWorkload(w.network, append([]string{name}, args...)...)...)
}

// explainGHOnFailure registers a cleanup that, when the test failed, prints
// what gh received for args: the same command run twice more in a row (each
// with its output and exit code), then run with GH_DEBUG=api and without
// --jq, so the status, headers and body show even when the filter prints
// nothing. It then prints the interceptor's log, the proxy's audit lines and
// the requests the proxy received. gh masks the Authorization header in its
// debug output, and none of these carry a token.
func (w *workload) explainGHOnFailure(t *testing.T, seen *authorizations, args ...string) {
	t.Helper()

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}

		for range 2 {
			_, _ = w.runSplit(t, nil, "/tmp", "gh", args...)
		}

		_, _ = w.runSplit(t, []string{"GH_DEBUG=api"}, "/tmp", "gh", withoutJQ(args)...)

		output, err := docker(t, "exec", w.name, "cat", interceptLogFile)
		w.live.log.printf(t, "interceptor log (err %v):\n%s", err, output)
		w.live.log.printf(t, "proxy audit lines:\n%s", strings.Join(w.live.proxyAudit.linesContaining(`"msg":"proxy request"`), "\n"))
		w.live.log.printf(t, "requests the proxy received:\n%s", strings.Join(seen.received(), "\n"))
	})
}

// withoutJQ drops a --jq flag and its filter from gh arguments.
func withoutJQ(args []string) []string {
	kept := make([]string, 0, len(args))

	for index := 0; index < len(args); index++ {
		if args[index] == "--jq" {
			index++

			continue
		}

		kept = append(kept, args[index])
	}

	return kept
}

func TestLiveInterceptionAgainstGitHub(t *testing.T) {
	live := setup(t)
	network := os.Getenv(dockerNetworkEnv)
	seen := &authorizations{}
	listener, caFile := live.serveProxy(t, proxyListenAddress(network), seen.observe)
	slug := live.owner + "/" + live.repo
	cloneURL := "https://github.com/" + slug + ".git"

	box := live.startWorkload(t, pushAccount, listener.Addr().String(), caFile, network)

	_, err := box.run(t, "/tmp", "mkdir", "-p", "/tmp/home")
	require.NoError(t, err)

	t.Run("gh api reads an allowed repository with no setup", func(t *testing.T) {
		args := []string{"api", "repos/" + slug, "--jq", ".name"}
		box.explainGHOnFailure(t, seen, args...)

		// The first gh call in the container is traced on stderr; only its
		// standard output is checked.
		out, err := box.runSplit(t, []string{"GH_DEBUG=api"}, "/tmp", "gh", args...)
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

		args := []string{"api", "repos/" + slug, "--jq", ".name", "-H", "Authorization: token " + foreignCredential}
		box.explainGHOnFailure(t, seen, args...)

		out, err = box.run(t, "/tmp", "gh", args...)
		require.NoError(t, err)
		assert.Equal(t, live.repo, strings.TrimSpace(out))

		for _, value := range seen.all() {
			assert.NotContains(t, value, foreignCredential, "the proxy received the foreign credential")
		}

		assert.NotEmpty(t, seen.all(), "the proxy saw the requests")
	})

	live.assertNoSecretsInTranscript(t)
}
