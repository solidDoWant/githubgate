//go:build live

package live_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

// interceptExecArgs are the docker arguments that start the interceptor.
func interceptExecArgs(name, network, port string) []string {
	return append([]string{"exec", "-d", name}, inWorkload(network, workloadWork+"/"+clientBinary, "intercept",
		"--listen", interceptListen,
		"--proxy-address", proxyAddress(network, port),
		"--proxy-ca", "@"+workloadWork+"/proxy-ca.crt",
		"--token-file", workloadWork+"/token",
		"--trust-dir", workloadTrust,
		"--state-dir", "/state")...)
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

	t.Run("hosts and listen address use the interceptor address and no bind mounts", func(t *testing.T) {
		assert.Equal(t, "127.43.0.1 api.github.com uploads.github.com github.com", hostsEntry())
		assert.Contains(t, interceptExecArgs("c", "", "1"), "127.43.0.1:443")
		assert.NotContains(t, workloadRunArgs("c", "img", ""), "-v")
	})
}
