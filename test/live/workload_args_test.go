//go:build live

package live_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proxyListenAddress is where the in-process proxy listens: loopback when the
// workload shares this machine's network, otherwise every interface so the
// container can reach it through the host gateway.
func proxyListenAddress(network string) string {
	if network != "" {
		return "127.0.0.1:0"
	}

	return "0.0.0.0:0"
}

// proxyAddress is how the workload reaches the proxy listening on port.
func proxyAddress(network, port string) string {
	if network != "" {
		return "127.0.0.1:" + port
	}

	return proxyHostName + ":" + port
}

// hostsEntry points the three GitHub names at the interceptor.
func hostsEntry() string {
	host, _, _ := strings.Cut(interceptListen, ":")

	return host + " api.github.com uploads.github.com github.com"
}

// workloadRunArgs are the docker arguments that start the idle container.
func workloadRunArgs(name, image, network string) []string {
	args := []string{"run", "-d", "--name", name}
	if network != "" {
		args = append(args, "--network", network)
	} else {
		args = append(args, "--add-host", proxyHostName+":host-gateway")
	}

	if network != "" {
		// A joined network shares the joined container's /etc/hosts, so each
		// tool gets a private copy inside its own mount namespace, which needs this.
		args = append(args, "--cap-add", "SYS_ADMIN")
	}

	return append(args, image, "sleep", "infinity")
}

// workloadHostsFile is the private hosts file used when the network is joined.
const workloadHostsFile = workloadWork + "/hosts"

// hostsSetupCommand is the shell command that makes the GitHub names resolve
// to the interceptor. On the bridge it edits the container's own /etc/hosts.
// On a joined network it only writes a private file, never /etc/hosts, which
// belongs to the joined container.
func hostsSetupCommand(network string) string {
	if network == "" {
		return "echo '" + hostsEntry() + "' >> /etc/hosts"
	}

	return "cp /etc/hosts " + workloadHostsFile + " && echo '" + hostsEntry() + "' >> " + workloadHostsFile
}

// inWorkload is the command to run in the container. On a joined network it
// runs in a new mount namespace with the private hosts file over /etc/hosts.
func inWorkload(network string, command ...string) []string {
	if network == "" {
		return command
	}

	return append([]string{"unshare", "-m", "sh", "-c",
		"mount --bind " + workloadHostsFile + " /etc/hosts && exec \"$@\"", "sh"}, command...)
}

// workloadRuns holds each command's output and exit code in the container.
const workloadRuns = workloadWork + "/runs"

// recordedCommand wraps a command so its shell writes the command's
// standard output to base.out, its standard error to base.err and, once the
// command has ended, its exit code to base.rc. The exit code file appears
// whole (written aside, then renamed), so a reader never sees half of it.
func recordedCommand(base string, command ...string) []string {
	script := `"$@" >"$0.out" 2>"$0.err"; echo $? >"$0.rc.tmp" && mv "$0.rc.tmp" "$0.rc"`

	return append([]string{"sh", "-c", script, base}, command...)
}

// interceptLogFile holds the interceptor's output inside the container.
const interceptLogFile = workloadWork + "/intercept.log"

// interceptSharedArgs are the options the interceptor and its readiness probe
// must agree on, so the probe looks where the interceptor works.
func interceptSharedArgs() []string {
	return []string{"--listen", interceptListen, "--trust-dir", workloadTrust}
}

// interceptExecArgs are the docker arguments that start the interceptor, with
// its output sent to interceptLogFile.
func interceptExecArgs(name, network, port string) []string {
	command := append([]string{workloadWork + "/" + clientBinary, "intercept"}, interceptSharedArgs()...)
	command = append(command,
		"--proxy-address", proxyAddress(network, port),
		"--proxy-ca", "@"+workloadWork+"/proxy-ca.crt",
		"--token-file", workloadWork+"/token",
		"--state-dir", "/state")
	logged := append([]string{"sh", "-c", "exec \"$@\" >" + interceptLogFile + " 2>&1", "sh"}, command...)

	return append([]string{"exec", "-d", name}, inWorkload(network, logged...)...)
}

// interceptProbeArgs are the docker arguments that check the interceptor is ready.
func interceptProbeArgs(name, network string) []string {
	command := append([]string{workloadWork + "/" + clientBinary, "intercept", "--probe"}, interceptSharedArgs()...)

	return append([]string{"exec", name}, inWorkload(network, command...)...)
}

func TestWorkloadDockerArguments(t *testing.T) {
	t.Run("default bridge uses the host gateway and listens on every interface", func(t *testing.T) {
		args := workloadRunArgs("c", "img", "")
		assert.Equal(t, []string{"run", "-d", "--name", "c", "--add-host", "githubgate-proxy.invalid:host-gateway", "img", "sleep", "infinity"}, args)
		assert.Equal(t, "0.0.0.0:0", proxyListenAddress(""))
		assert.Contains(t, interceptExecArgs("c", "", "4000"), "githubgate-proxy.invalid:4000")
	})

	t.Run("a joined network reaches the proxy on loopback", func(t *testing.T) {
		args := workloadRunArgs("c", "img", "container:abc")
		assert.Equal(t, []string{"run", "-d", "--name", "c", "--network", "container:abc", "--cap-add", "SYS_ADMIN", "img", "sleep", "infinity"}, args)
		assert.NotContains(t, strings.Join(args, " "), "host-gateway")
		assert.Equal(t, "127.0.0.1:0", proxyListenAddress("container:abc"))
		assert.Contains(t, interceptExecArgs("c", "container:abc", "4000"), "127.0.0.1:4000")
	})

	t.Run("a joined network never writes the shared hosts file", func(t *testing.T) {
		args := workloadRunArgs("c", "img", "container:abc")
		assert.Contains(t, strings.Join(args, " "), "--cap-add SYS_ADMIN")
		assert.NotContains(t, strings.Join(workloadRunArgs("c", "img", ""), " "), "SYS_ADMIN")

		setup := hostsSetupCommand("container:abc")
		assert.NotContains(t, setup, ">> /etc/hosts")
		assert.Contains(t, setup, ">> "+workloadHostsFile)
		assert.Contains(t, hostsSetupCommand(""), ">> /etc/hosts")

		wrapped := strings.Join(inWorkload("container:abc", "tool", "a"), " ")
		assert.Contains(t, wrapped, "unshare -m")
		assert.Contains(t, wrapped, "mount --bind "+workloadHostsFile+" /etc/hosts")
		assert.Equal(t, []string{"tool", "a"}, inWorkload("", "tool", "a"))

		assert.Contains(t, strings.Join(interceptExecArgs("c", "container:abc", "1"), " "), "unshare -m")
	})

	t.Run("the probe checks the same listen address and trust directory as the interceptor", func(t *testing.T) {
		for _, network := range []string{"", "container:abc"} {
			start := strings.Join(interceptExecArgs("c", network, "1"), " ")
			probe := strings.Join(interceptProbeArgs("c", network), " ")
			shared := strings.Join(interceptSharedArgs(), " ")

			assert.Equal(t, "--listen "+interceptListen+" --trust-dir "+workloadTrust, shared)
			assert.Contains(t, start, shared)
			assert.Contains(t, probe, "intercept --probe "+shared)
		}
	})

	t.Run("a recorded command leaves its output and exit code in files once it ends", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "1")
		command := recordedCommand(base, "sh", "-c", "echo out; echo err >&2; exit 3")

		require.NoError(t, exec.CommandContext(t.Context(), command[0], command[1:]...).Run())

		for suffix, want := range map[string]string{".out": "out\n", ".err": "err\n", ".rc": "3\n"} {
			got, err := os.ReadFile(base + suffix)
			require.NoError(t, err)
			assert.Equal(t, want, string(got), suffix)
		}

		assert.NoFileExists(t, base+".rc.tmp")
	})

	t.Run("hosts and listen address use the interceptor address and no bind mounts", func(t *testing.T) {
		assert.Equal(t, "127.43.0.1 api.github.com uploads.github.com github.com", hostsEntry())
		assert.Contains(t, interceptExecArgs("c", "", "1"), "127.43.0.1:443")
		assert.NotContains(t, workloadRunArgs("c", "img", ""), "-v")
	})
}
