//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	vmPod = "ci-vm"
	// The paths the test NixOS configuration in flake.nix gives the module.
	vmProxyCADir = "/var/run/githubgate-proxy"
	vmExtraCADir = "/var/run/githubgate-extra"
)

// vmSettings is what the NixOS module sets for a VM, read from the evaluated
// test configuration.
type vmSettings struct {
	ExecStart string            `json:"execStart"`
	Hosts     []string          `json:"hosts"`
	Env       map[string]string `json:"env"`
	Tmpfiles  []string          `json:"tmpfiles"`
}

// evalVMSettings evaluates the flake's test NixOS configuration, which uses
// the module, and returns the service command, the host names and the
// environment variables the module sets. Nothing is built.
func evalVMSettings(ctx context.Context, t *testing.T) vmSettings {
	t.Helper()

	if _, err := exec.LookPath("nix"); err != nil {
		t.Skip("nix is not installed")
	}

	root, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)

	out, err := exec.CommandContext(ctx, "nix", "--extra-experimental-features", "nix-command flakes", "eval", "--json",
		"git+file://"+strings.TrimSpace(string(root))+"#nixosConfigurations.githubgate-test-vm.config",
		"--apply", `c: {
			execStart = c.systemd.services.githubgate-intercept.serviceConfig.ExecStart;
			hosts = c.networking.hosts."127.0.0.1";
			env = c.environment.variables;
			tmpfiles = c.systemd.tmpfiles.rules;
		}`).Output()
	require.NoError(t, err)

	var settings vmSettings
	require.NoError(t, json.Unmarshal(out, &settings))

	return settings
}

// startVMPod runs a pod that stands in for a VM: the three GitHub host names
// point at 127.0.0.1, as the module sets them, and the proxy CA and the extra
// CA are at the paths the module was given. It runs as root so that the
// interceptor can bind port 443 (the module grants the service user
// CAP_NET_BIND_SERVICE instead).
func (c cluster) startVMPod(t *testing.T, hosts []string) {
	t.Helper()

	extraCA := c.kubectl(t, "", "exec", "-n", "fake", "deploy/fakegithub", "--", "cat", "/tmp/fake-ca.crt")
	c.kubectl(t, extraCA, "create", "configmap", "vm-extra-ca", "-n", callerNamespace, "--from-file=ca.crt=/dev/stdin")

	c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: %[1]s, namespace: %[2]s}
---
apiVersion: v1
kind: Pod
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  serviceAccountName: %[1]s
  hostAliases:
    - ip: 127.0.0.1
      hostnames: [%[5]s]
    - ip: %[6]s
      hostnames: [%[7]s]
  containers:
    - name: main
      image: %[3]s
      imagePullPolicy: IfNotPresent
      command: [sleep, infinity]
      securityContext: {runAsUser: 0}
      env:
        - {name: HOME, value: /tmp}
      volumeMounts:
        - {name: githubgate, mountPath: /var/run/secrets/githubgate, readOnly: true}
        - {name: client, mountPath: /opt/client, readOnly: true}
        - {name: proxy-ca, mountPath: %[8]s, readOnly: true}
        - {name: extra-ca, mountPath: %[9]s, readOnly: true}
  volumes:
    - name: githubgate
      projected:
        sources:
          - serviceAccountToken: {audience: githubgate, path: token, expirationSeconds: 3600}
    - name: client
      image: {reference: %[4]s, pullPolicy: IfNotPresent}
    - name: proxy-ca
      configMap: {name: githubgate-proxy-ca}
    - name: extra-ca
      configMap: {name: vm-extra-ca}
`, vmPod, callerNamespace, c.fakeImage, c.clientImage, strings.Join(hosts, ", "), fakeTLSIP, fakegithub.StorageHost, vmProxyCADir, vmExtraCADir))
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", callerNamespace, "pod/"+vmPod, "--timeout=3m")
}

// vmWorkspaceScenarios starts the module's service command in a pod and runs
// gh, git and curl in it with the module's environment and nothing else.
func (c cluster) vmWorkspaceScenarios(ctx context.Context, t *testing.T) {
	settings := evalVMSettings(ctx, t)

	require.Len(t, settings.Hosts, 3)
	assert.True(t, slices.ContainsFunc(settings.Tmpfiles, func(rule string) bool {
		return strings.HasPrefix(rule, "C ") && strings.Contains(rule, "/bundle.crt ")
	}), "a tmpfiles rule copies the system bundle to bundle.crt: %v", settings.Tmpfiles)

	// The command is `<store path>/bin/githubgate-client intercept <flags>`;
	// the test runs the same arguments with the e2e client binary.
	fields := strings.Fields(settings.ExecStart)
	require.Greater(t, len(fields), 2)
	require.Equal(t, "intercept", fields[1])

	c.startVMPod(t, settings.Hosts)

	// The module's service user, runtime directory and tmpfiles rule are
	// systemd's job; the test makes the directories they would make.
	var trustDir, stateDir string

	for _, field := range fields[2:] {
		switch {
		case strings.HasPrefix(field, "--trust-dir="):
			trustDir = strings.TrimPrefix(field, "--trust-dir=")
		case strings.HasPrefix(field, "--state-dir="):
			stateDir = strings.TrimPrefix(field, "--state-dir=")
		}
	}

	require.NotEmpty(t, trustDir)
	require.NotEmpty(t, stateDir)

	command := clientBinary + " " + strings.Join(fields[1:], " ")
	start := fmt.Sprintf(`set -e
mkdir -p %[1]s %[2]s
chmod 0700 %[2]s
(setsid %[3]s >/tmp/intercept.log 2>&1 &)
for i in $(seq 1 100); do
  if %[4]s intercept --listen=127.0.0.1:443 --trust-dir=%[1]s --probe 2>/dev/null; then exit 0; fi
  sleep 0.2
done
cat /tmp/intercept.log
exit 1
`, trustDir, stateDir, command, clientBinary)
	out, err := c.exec(ctx, callerNamespace, vmPod, start)
	require.NoError(t, err, out)

	names := make([]string, 0, len(settings.Env))
	for name := range settings.Env {
		names = append(names, name)
	}

	slices.Sort(names)

	var env strings.Builder
	for _, name := range names {
		fmt.Fprintf(&env, "export %s=%q\n", name, settings.Env[name])
	}

	// inVM runs a script as a login shell of the VM would: the module's
	// variables, no credential helper and no other setup.
	inVM := func(t *testing.T, script string) string {
		t.Helper()

		out, err := c.exec(ctx, callerNamespace, vmPod, "set -e\nexport GIT_TERMINAL_PROMPT=0\n"+env.String()+script)
		require.NoError(t, err, out)

		return out
	}

	t.Run("gh api reads an allowed repository", func(t *testing.T) {
		out := inVM(t, fmt.Sprintf("gh api repos/%s/%s --jq .name\n", owner, repository))
		assert.Equal(t, repository, strings.TrimSpace(out))
	})

	t.Run("git clones, fetches and pushes an allowed repository", func(t *testing.T) {
		out := inVM(t, fmt.Sprintf(`git config --global user.name e2e
git config --global user.email e2e@example.com
rm -rf /tmp/vm-clone
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/vm-clone
cd /tmp/vm-clone
git fetch -q origin
git checkout -q -b vm-branch
echo vm >vm.txt
git add vm.txt
git commit -q -m "from the VM"
git push -q origin vm-branch
test "$(git ls-remote origin refs/heads/vm-branch | cut -f1)" = "$(git rev-parse HEAD)"
echo pushed
`, owner, repository))
		assert.Contains(t, out, "pushed")
	})

	t.Run("curl fetches an allowed repository with no token", func(t *testing.T) {
		out := inVM(t, fmt.Sprintf("curl -sS https://api.github.com/repos/%s/%s\n", owner, repository))
		assert.Contains(t, out, fmt.Sprintf(`"name":"%s"`, repository))
	})
}
