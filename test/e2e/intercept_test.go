//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	interceptNamespace = "intercept"
	plainNamespace     = "intercept-plain"
	downNamespace      = "intercept-down"
	interceptAccount   = "ci-intercept"
	trustDirectory     = "/var/run/githubgate/trust"
	rejectPrefix       = "githubgate: pod can't be intercepted: "
	personalToken      = "ghp_e2ePersonalAccessTokenThatMustNeverLeave"
)

// installBase installs the fake GitHub, githubgate with its proxy and the App,
// installation and policies, unless an earlier test did.
func (c cluster) installBase(t *testing.T) {
	t.Helper()

	if _, err := c.tryKubectl(t.Context(), "", "get", "deployment/githubgate", "-n", gateNamespace); err == nil {
		return
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		c.installFakeGitHub(t)
	}()

	c.installGatewayNamespaceAndProxyCA(t)
	<-done

	c.installGitHubgate(t)
	c.configure(t)
}

// enableInjection turns the injection webhook on, with the proxy's CA (from
// the proxy certificate's Secret) and the fake's test CA for the injected pods.
func (c cluster) enableInjection(t *testing.T) {
	t.Helper()

	require.NotEmpty(t, c.clientDigest, "E2E_CLIENT_IMAGE_DIGEST is set by test/e2e/up.sh")

	repo, tag, _ := strings.Cut(c.serverImage, ":")
	clientRepo, _ := splitImage(c.clientImage)

	c.helm(t, "upgrade", "--install", "githubgate", "../../deploy/charts/githubgate",
		"--namespace", gateNamespace, "--reuse-values", "--set", "image.repository="+repo, "--set", "image.tag="+tag,
		"--set", "inject.enabled=true",
		"--set", "inject.image.repository="+clientRepo, "--set", "inject.image.digest="+c.clientDigest,
		"--set", "inject.extraCAConfigMap=fake-github-ca",
		"--wait", "--timeout", "4m")

	eventually(t, "the injection webhook has its CA", func() bool {
		out, _ := c.tryKubectl(t.Context(), "", "get", "mutatingwebhookconfiguration/githubgate-inject", "-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")

		return strings.TrimSpace(out) != ""
	}, 2*time.Minute, time.Second)
}

// applyConfigMap creates or replaces a ConfigMap whose ca.crt holds data.
func (c cluster) applyConfigMap(t *testing.T, namespace, name, data string) {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"data":     map[string]string{"ca.crt": data},
	})
	require.NoError(t, err)
	c.apply(t, string(raw))
}

// interceptContainer is a container of a test pod: it runs script with bash.
type interceptContainer struct {
	name, script string
	env          string // YAML list items, indented for a container's env
	extra        string // YAML lines at the container's level
}

func (container interceptContainer) yaml(image string) string {
	env := ""
	if container.env != "" {
		env = "      env:\n" + container.env
	}

	return fmt.Sprintf("    - name: %s\n      image: %s\n      imagePullPolicy: IfNotPresent\n      command: [bash, -c, %q]\n%s%s",
		container.name, image, container.script, env, container.extra)
}

// interceptPod describes a plain pod: nothing of githubgate's is set up in it.
type interceptPod struct {
	name, namespace string
	annotations     string // YAML, indented under metadata
	labels          string // YAML, indented under metadata
	spec            string // YAML lines at the pod spec's level
	initContainers  []interceptContainer
	containers      []interceptContainer
}

// firstRequest runs once as the container starts and records the outcome of
// the pod's first GitHub request.
const firstRequest = `gh api repos/acme/project --jq .name >/tmp/first.out 2>&1; echo $? >/tmp/first.status; sleep infinity`

func (c cluster) interceptManifest(pod interceptPod) string {
	if len(pod.containers) == 0 {
		pod.containers = []interceptContainer{{name: "main", script: firstRequest}}
	}

	var init, containers strings.Builder

	for _, container := range pod.initContainers {
		init.WriteString(container.yaml(c.fakeImage))
	}

	for _, container := range pod.containers {
		containers.WriteString(container.yaml(c.fakeImage))
	}

	initBlock := ""
	if init.Len() > 0 {
		initBlock = "  initContainers:\n" + init.String()
	}

	// kubectl exec and logs then pick the pod's own container, not the sidecar.
	annotations := "  annotations:\n    kubectl.kubernetes.io/default-container: main\n" + pod.annotations
	if pod.labels != "" {
		annotations += "  labels:\n" + pod.labels
	}

	// The storage host is not one of the three intercepted names; it reaches
	// the fake directly, as GitHub's storage hosts are reached directly.
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata: {name: %[3]s, namespace: %[2]s}
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
%[4]sspec:
  serviceAccountName: %[3]s
%[5]s%[6]s  containers:
%[7]s`, pod.name, pod.namespace, interceptAccount, annotations, pod.spec+c.storageAlias(pod.spec), initBlock, containers.String())
}

func (c cluster) storageAlias(spec string) string {
	if strings.Contains(spec, "hostAliases") {
		return ""
	}

	return fmt.Sprintf("  hostAliases:\n    - ip: %s\n      hostnames: [%s]\n", fakeTLSIP, fakegithub.StorageHost)
}

func (c cluster) startInterceptPod(t *testing.T, pod interceptPod) {
	t.Helper()

	c.apply(t, c.interceptManifest(pod))
	c.waitPodReady(t, pod.namespace, pod.name)
}

func (c cluster) podJSON(t *testing.T, namespace, name string) map[string]any {
	t.Helper()

	var pod map[string]any

	require.NoError(t, json.Unmarshal([]byte(c.kubectl(t, "", "get", "pod", "-n", namespace, name, "-o", "json")), &pod))

	return pod
}

// initContainerNames returns the names of the pod's init containers.
func (c cluster) initContainerNames(t *testing.T, namespace, name string) []string {
	t.Helper()

	return strings.Fields(c.kubectl(t, "", "get", "pod", "-n", namespace, name, "-o", "jsonpath={.spec.initContainers[*].name}"))
}

func TestInterceptionOnKind(t *testing.T) {
	c := clusterFromEnv(t)

	ctx := t.Context()

	c.installBase(t)
	c.enableInjection(t)

	// The opted-in namespace, one without the label, and one used to take the
	// webhook away. Each has its own ServiceAccount named like the policy's.
	c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata: {name: %[1]s, labels: {e2e-client: "true", githubgate.soliddowant.dev/inject: "true"}}
---
apiVersion: v1
kind: Namespace
metadata: {name: %[2]s, labels: {e2e-client: "true"}}
---
apiVersion: v1
kind: Namespace
metadata: {name: %[3]s, labels: {e2e-webhook-down: "true"}}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: intercept, namespace: %[1]s}
spec:
  serviceAccounts: [%[4]s]
  rules:
    - name: proxy
      installationRef: {namespace: %[5]s, name: acme}
      repositories: [%[6]s, %[7]s]
      permissions: {contents: write, issues: write}
      proxy:
        enabled: true
        pushRefs: [refs/heads/agent/**, refs/tags/v-*]
        publicRead: true
        webPaths: true
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: intercept, namespace: %[2]s}
spec:
  serviceAccounts: [%[4]s]
  rules:
    - name: proxy
      installationRef: {namespace: %[5]s, name: acme}
      repositories: [%[6]s]
      permissions: {contents: read}
      proxy:
        enabled: true
`, interceptNamespace, plainNamespace, downNamespace, interceptAccount, gateNamespace, repository, publicRepository))
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", interceptNamespace, "githubaccesspolicy/intercept", "--timeout=2m")
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", plainNamespace, "githubaccesspolicy/intercept", "--timeout=2m")

	// Names a rerun on the same cluster must not collide with.
	run := time.Now().UnixNano()
	branch := fmt.Sprintf("agent/intercept-%d", run)
	tag := fmt.Sprintf("v-intercept-%d", run)

	// plain is the pod with no githubgate setup. Its first request runs as it starts.
	c.startInterceptPod(t, interceptPod{name: "plain", namespace: interceptNamespace})

	t.Run("a pod with no setup of its own", func(t *testing.T) {
		t.Run("its first request succeeds because the interception is ready before its containers start", func(t *testing.T) {
			eventually(t, "the first request ran", func() bool {
				out, err := c.exec(ctx, interceptNamespace, "plain", "cat /tmp/first.status")

				return err == nil && strings.TrimSpace(out) != ""
			}, time.Minute, 500*time.Millisecond)

			status, err := c.exec(ctx, interceptNamespace, "plain", "cat /tmp/first.status")
			require.NoError(t, err)

			output, _ := c.exec(ctx, interceptNamespace, "plain", "cat /tmp/first.out")
			assert.Equal(t, "0", strings.TrimSpace(status), "the first request failed: %s", output)
			assert.Equal(t, repository, strings.TrimSpace(output))

			names := c.initContainerNames(t, interceptNamespace, "plain")
			require.NotEmpty(t, names)
			assert.Equal(t, "githubgate-intercept", names[0], "the sidecar starts first")
		})

		t.Run("gh api returns GitHub's answer for a repository the rule covers", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf("gh api repos/%s/%s --jq .full_name", owner, repository))
			require.NoError(t, err, out)
			assert.Equal(t, owner+"/"+repository, strings.TrimSpace(out))
		})

		t.Run("curl without a token gets GitHub's answer", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf("curl -sS -w '\\nstatus=%%{http_code}' https://api.github.com/repos/%s/%s", owner, repository))
			require.NoError(t, err, out)
			assert.Contains(t, out, "status=200")
			assert.Contains(t, out, `"name"`)
		})

		t.Run("a repository the rule does not cover is refused", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf("curl -sS -w '\\nstatus=%%{http_code}' https://api.github.com/repos/%s/%s", owner, otherRepository))
			require.NoError(t, err, out)
			assert.Contains(t, out, "status=403")
		})

		t.Run("git clone, fetch and push work over HTTPS with no credential helper", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf(`set -e
export GIT_TERMINAL_PROMPT=0
if git config --get-all credential.helper; then echo "a credential helper is configured"; exit 1; fi
rm -rf /tmp/one /tmp/two
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/one
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/two
cd /tmp/one
git -c user.name=e2e -c user.email=e2e@example.com commit -q --allow-empty -m pushed-by-the-pod
git push -q origin HEAD:refs/heads/%[3]s
sha=$(git rev-parse HEAD)
cd /tmp/two
git fetch -q origin
test "$(git rev-parse origin/%[3]s)" = "$sha"
`, owner, repository, branch))
			require.NoError(t, err, out)
		})

		t.Run("a public repository of another owner can be cloned and read", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf(`set -e
export GIT_TERMINAL_PROMPT=0
rm -rf /tmp/public
git clone -q https://github.com/%[1]s/public.git /tmp/public 2>&1
gh api repos/%[1]s/public --jq .full_name
`, outsider))
			require.NoError(t, err, out)
			assert.Contains(t, out, outsider+"/public")
		})

		t.Run("a private repository of another owner gets GitHub's 404", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf("curl -sS -w '\\nstatus=%%{http_code}' https://api.github.com/repos/%s/secret", outsider))
			require.NoError(t, err, out)
			assert.Contains(t, out, "status=404")
		})

		t.Run("a write to another owner's repository is refused naming it", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf("curl -sS -X DELETE -w '\\nstatus=%%{http_code}' https://api.github.com/repos/%s/public", outsider))
			require.NoError(t, err, out)
			assert.Contains(t, out, "status=403")
			assert.Contains(t, out, outsider+"/public")
		})

		t.Run("a release download from github.com follows the redirect with no credential", func(t *testing.T) {
			// The fake refuses a github.com web path that carries any credential.
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf(`set -e
id=$(gh api -X POST repos/%[1]s/%[2]s/releases -f tag_name=v-web-%[3]d --jq .id)
curl -sS -f -X POST -H 'Content-Type: text/plain' --data-binary web-asset-bytes -o /dev/null "https://uploads.github.com/repos/%[1]s/%[2]s/releases/${id}/assets?name=tool.txt"
curl -sS -f -L https://github.com/%[1]s/%[2]s/releases/latest/download/tool.txt
`, owner, publicRepository, run))
			require.NoError(t, err, out)
			assert.Contains(t, out, "web-asset-bytes")
		})

		t.Run("a push to a ref outside the rule is refused", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", `cd /tmp/one && GIT_TERMINAL_PROMPT=0 git push origin HEAD:refs/heads/master 2>&1`)
			require.Error(t, err, out)
			assert.Contains(t, out, "pushRefs")
		})

		t.Run("gh release download and an archive download follow the redirect to the storage host", func(t *testing.T) {
			out, err := c.exec(ctx, interceptNamespace, "plain", fmt.Sprintf(`set -e
id=$(gh api -X POST repos/%[1]s/%[2]s/releases -f tag_name=%[3]s --jq .id)
curl -sS -f -X POST -H 'Content-Type: text/plain' --data-binary release-asset-bytes -o /dev/null "https://uploads.github.com/repos/%[1]s/%[2]s/releases/${id}/assets?name=tool.txt"
rm -rf /tmp/dl /tmp/archive.tar
gh release download %[3]s -R %[1]s/%[2]s -p tool.txt -D /tmp/dl
cat /tmp/dl/tool.txt
echo
curl -sS -f -L -o /tmp/archive.tar -w 'archive status=%%{http_code} redirects=%%{num_redirects}' https://api.github.com/repos/%[1]s/%[2]s/tarball/%[4]s
`, owner, repository, tag, branch))
			require.NoError(t, err, out)
			assert.Contains(t, out, "release-asset-bytes")
			assert.Contains(t, out, "archive status=200 redirects=1")
		})
	})

	// Each subtest starts its own pod, so they wait for pods to start together.
	t.Run("pods that start on their own", func(t *testing.T) {
		t.Run("githubgate-client push in an intercepted pod publishes inside pushRefs and refuses outside it", func(t *testing.T) {
			t.Parallel()

			c.startInterceptPod(t, interceptPod{
				name: "signer", namespace: interceptNamespace,
				spec: fmt.Sprintf("  volumes:\n    - name: client\n      image: {reference: %s, pullPolicy: IfNotPresent}\n", c.clientImage),
				containers: []interceptContainer{{
					name: "main", script: "sleep infinity",
					extra: "      volumeMounts:\n        - {name: client, mountPath: /opt/client, readOnly: true}\n",
				}},
			})

			out, err := c.exec(ctx, interceptNamespace, "signer", fmt.Sprintf(`export PATH=%s:$PATH GIT_TERMINAL_PROMPT=0
set -e
git config --global user.name e2e
git config --global user.email e2e@example.com
rm -rf /tmp/signer
git clone -q https://github.com/%s/%s.git /tmp/signer
cd /tmp/signer
git checkout -q -b agent/signed-%d
echo signed >signed.txt
git add signed.txt
git commit -q -m "signed through the interceptor"
githubgate-client push --intercepted
test "$(git ls-remote origin refs/heads/agent/signed-%d | cut -f1)" = "$(git rev-parse HEAD)"
git checkout -q -b outside-%d
echo outside >outside.txt
git add outside.txt
git commit -q -m "outside pushRefs"
if githubgate-client push --intercepted 2>&1; then echo "published outside pushRefs"; exit 1; fi
if [ -n "$(git ls-remote origin refs/heads/outside-%d)" ]; then echo "outside reached GitHub"; exit 1; fi
`, path.Dir(clientBinary), owner, repository, run, run, run, run))
			require.NoError(t, err, out)
			assert.Contains(t, out, "published 1 commit(s)")
			assert.Contains(t, out, "pushRefs")
		})

		t.Run("a credential the pod brings never reaches GitHub", func(t *testing.T) {
			t.Parallel()

			c.startInterceptPod(t, interceptPod{
				name: "own-token", namespace: interceptNamespace,
				containers: []interceptContainer{{
					name: "main", script: "sleep infinity",
					env: fmt.Sprintf("        - {name: GH_TOKEN, value: %s}\n", personalToken),
				}},
			})

			out, err := c.exec(ctx, interceptNamespace, "own-token", fmt.Sprintf(`set -e
test "$GH_TOKEN" = %[1]s
gh api repos/%[2]s/%[3]s --jq .name
export GIT_TERMINAL_PROMPT=0
rm -rf /tmp/helper
git -c credential.helper='!f() { echo username=x-access-token; echo password=%[1]s; }; f' clone -q https://github.com/%[2]s/%[3]s.git /tmp/helper
echo cloned
`, personalToken, owner, repository))
			require.NoError(t, err, "the pod's own token must not break requests, the interceptor replaces it: %s", out)
			assert.Contains(t, out, repository)
			assert.Contains(t, out, "cloned")

			// The fake accepts only tokens it minted, so success means the pod's
			// requests were made with the ServiceAccount's minted token.
			corpus := c.kubectl(t, "", "logs", "-n", "fake", "deploy/fakegithub", "--tail=-1")
			corpus += c.kubectl(t, "", "logs", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "--all-containers", "--tail=-1")
			assert.NotContains(t, corpus, personalToken, "the pod's own token reached githubgate or the fake GitHub")
		})

		t.Run("namespaces and pods that are not intercepted", func(t *testing.T) {
			t.Parallel()

			t.Run("a pod in a namespace without the label is created unchanged", func(t *testing.T) {
				t.Parallel()

				c.apply(t, c.interceptManifest(interceptPod{name: "plain-ns", namespace: plainNamespace, containers: []interceptContainer{{name: "main", script: "sleep infinity"}}}))
				assert.Empty(t, c.initContainerNames(t, plainNamespace, "plain-ns"))
				assert.Empty(t, c.kubectl(t, "", "get", "pod", "-n", plainNamespace, "plain-ns", "-o", "jsonpath={.metadata.annotations.githubgate\\.soliddowant\\.dev/injected}"))
				assert.Empty(t, c.kubectl(t, "", "get", "pod", "-n", plainNamespace, "plain-ns", "-o", "jsonpath={.spec.containers[0].env}"), "no environment was added")
			})
			t.Run("a pod with the opt-out annotation in an opted-in namespace is created unchanged", func(t *testing.T) {
				t.Parallel()

				c.apply(t, c.interceptManifest(interceptPod{
					name: "opt-out", namespace: interceptNamespace,
					annotations: "    githubgate.soliddowant.dev/inject: \"false\"\n",
					containers:  []interceptContainer{{name: "main", script: "sleep infinity"}},
				}))
				assert.Empty(t, c.initContainerNames(t, interceptNamespace, "opt-out"))
				assert.Empty(t, c.kubectl(t, "", "get", "pod", "-n", interceptNamespace, "opt-out", "-o", "jsonpath={.spec.containers[0].env}"))
			})
		})

		t.Run("a pod opted in by its own label with a listen annotation is intercepted on that address", func(t *testing.T) {
			t.Parallel()

			// Another loopback address stands in for a KubeVirt launcher pod's
			// guest gateway.
			const listenIP = "127.43.0.2"

			c.startInterceptPod(t, interceptPod{
				name: "pod-label", namespace: plainNamespace,
				labels:      "    githubgate.soliddowant.dev/inject: \"true\"\n",
				annotations: "    githubgate.soliddowant.dev/listen: \"" + listenIP + ":443\"\n",
				containers:  []interceptContainer{{name: "main", script: "sleep infinity"}},
			})

			names := c.initContainerNames(t, plainNamespace, "pod-label")
			require.NotEmpty(t, names)
			assert.Equal(t, "githubgate-intercept", names[0])
			assert.Contains(t, c.kubectl(t, "", "get", "pod", "-n", plainNamespace, "pod-label", "-o", "jsonpath={.spec.hostAliases}"), `"ip":"`+listenIP+`"`)

			out, err := c.exec(ctx, plainNamespace, "pod-label", fmt.Sprintf(`set -e
grep api.github.com /etc/hosts
curl -sS -w '\nstatus=%%{http_code}' https://api.github.com/repos/%s/%s`, owner, repository))
			require.NoError(t, err, out)
			assert.Contains(t, out, listenIP)
			assert.Contains(t, out, "status=200")
		})

		t.Run("a pod that cannot be intercepted safely is rejected naming the reason", func(t *testing.T) {
			t.Parallel()

			tests := map[string]struct {
				pod    interceptPod
				reason string
			}{
				"host network": {
					pod:    interceptPod{name: "reject-host-network", spec: "  hostNetwork: true\n"},
					reason: "hostNetwork is true",
				},
				"host aliases for a GitHub host": {
					pod:    interceptPod{name: "reject-host-alias", spec: "  hostAliases:\n    - ip: 10.1.2.3\n      hostnames: [api.github.com]\n"},
					reason: "hostAliases names api.github.com",
				},
				"a CA variable in the environment": {
					pod: interceptPod{name: "reject-ca-env", containers: []interceptContainer{{
						name: "main", script: "sleep infinity", env: "        - {name: SSL_CERT_FILE, value: /etc/ssl/own.crt}\n",
					}}},
					reason: `container "main" sets SSL_CERT_FILE to a file githubgate can't read`,
				},
				"port 443": {
					pod: interceptPod{name: "reject-port", containers: []interceptContainer{{
						name: "main", script: "sleep infinity", extra: "      ports: [{containerPort: 443}]\n",
					}}},
					reason: "container main uses port 443",
				},
			}

			for name, tt := range tests {
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					tt.pod.namespace = interceptNamespace

					out, err := c.tryKubectl(ctx, c.interceptManifest(tt.pod), "apply", "-f", "-")
					require.Error(t, err, "the pod must be rejected: %s", out)
					assert.Contains(t, out, rejectPrefix+tt.reason)

					_, getErr := c.tryKubectl(ctx, "", "get", "pod", "-n", interceptNamespace, tt.pod.name)
					assert.Error(t, getErr, "a rejected pod must not exist")
				})
			}
		})

		t.Run("a pod whose CA variable points into a mounted volume runs and trusts both CAs", func(t *testing.T) {
			t.Parallel()

			// A second fake GitHub with a CA of its own stands in for a server the
			// pod's own CA signs. It answers to codeload.github.com, a name the
			// interceptor does not serve, so the pod reaches it directly.
			c.apply(t, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: own-ca-server, namespace: fake}
spec:
  replicas: 1
  selector: {matchLabels: {app: own-ca-server}}
  template:
    metadata: {labels: {app: own-ca-server}}
    spec:
      containers:
        - name: server
          image: %[1]s
          imagePullPolicy: IfNotPresent
          args: [--tls-address=:8443, --tls-ca-out=/tmp/own-ca.crt]
          readinessProbe: {tcpSocket: {port: 8443}}
---
apiVersion: v1
kind: Service
metadata: {name: own-ca-server, namespace: fake}
spec:
  selector: {app: own-ca-server}
  ports: [{name: https, port: 443, targetPort: 8443}]
`, c.fakeImage))
			c.kubectl(t, "", "rollout", "status", "-n", "fake", "deployment/own-ca-server", "--timeout=3m")

			ownCA := c.kubectl(t, "", "exec", "-n", "fake", "deploy/own-ca-server", "--", "cat", "/tmp/own-ca.crt")
			c.applyConfigMap(t, interceptNamespace, "own-ca", ownCA)

			c.startInterceptPod(t, interceptPod{
				name: "own-ca", namespace: interceptNamespace,
				spec: "  volumes:\n    - name: own-ca\n      configMap: {name: own-ca}\n",
				containers: []interceptContainer{{
					name: "main", script: "sleep infinity",
					env:   "        - {name: SSL_CERT_FILE, value: /etc/own/ca.crt}\n",
					extra: "      volumeMounts:\n        - {name: own-ca, mountPath: /etc/own, readOnly: true}\n",
				}},
			})

			// gh reads the variable, so its request through the interceptor needs the
			// pod's CA in the joined file. curl is given the same file explicitly,
			// because it prefers the bundle variable the webhook sets.
			out, err := c.exec(ctx, interceptNamespace, "own-ca", fmt.Sprintf(`set -e
gh api repos/%[1]s/%[2]s --jq .name
ip=$(getent hosts own-ca-server.fake.svc | cut -d' ' -f1)
test -n "$ip"
curl -sS -o /dev/null -w 'own status=%%{http_code}' --cacert "$SSL_CERT_FILE" --resolve codeload.github.com:443:$ip https://codeload.github.com/
`, owner, repository))
			require.NoError(t, err, "both CAs must be trusted from the container: %s", out)
			assert.Contains(t, out, repository)
			assert.Contains(t, out, "own status=")
		})

		t.Run("a Nix-style container that reads only NIX_SSL_CERT_FILE verifies the interceptor", func(t *testing.T) {
			t.Parallel()

			c.startInterceptPod(t, interceptPod{name: "nix-style", namespace: interceptNamespace})

			// Nix's OpenSSL prefers NIX_SSL_CERT_FILE over the other variables, so the
			// request names that file alone and ignores every other trust variable.
			out, err := c.exec(ctx, interceptNamespace, "nix-style", fmt.Sprintf(
				`test "$NIX_SSL_CERT_FILE" = /var/run/githubgate/trust/bundle.crt && env -u SSL_CERT_FILE -u CURL_CA_BUNDLE -u GIT_SSL_CAINFO curl -sS --cacert "$NIX_SSL_CERT_FILE" -w '\nstatus=%%{http_code}' https://api.github.com/repos/%s/%s`, owner, repository))
			require.NoError(t, err, out)
			assert.Contains(t, out, "status=200")
		})

		t.Run("pods are not created while the injection webhook is unavailable", func(t *testing.T) {
			t.Parallel()

			// A second webhook with the same shape, pointing at a Service with no
			// endpoints, for a label of its own. githubgate's webhook keeps running.
			c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata: {name: nobody, namespace: %[1]s}
spec:
  ports: [{port: 443}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata: {name: e2e-unavailable}
webhooks:
  - name: unavailable.e2e.githubgate.soliddowant.dev
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    timeoutSeconds: 2
    clientConfig:
      service: {name: nobody, namespace: %[1]s, path: /inject-pod}
    namespaceSelector: {matchLabels: {e2e-webhook-down: "true"}}
    rules:
      - {apiGroups: [""], apiVersions: ["v1"], operations: ["CREATE"], resources: ["pods"]}
`, downNamespace))
			t.Cleanup(func() {
				_, _ = c.tryKubectl(context.WithoutCancel(ctx), "", "delete", "mutatingwebhookconfiguration/e2e-unavailable", "--ignore-not-found")
			})

			out, err := c.tryKubectl(ctx, c.interceptManifest(interceptPod{name: "down", namespace: downNamespace}), "apply", "-f", "-")
			require.Error(t, err, "a pod must not be created while its webhook is unavailable: %s", out)
			assert.Contains(t, out, "failed calling webhook")

			_, getErr := c.tryKubectl(ctx, "", "get", "pod", "-n", downNamespace, "down")
			assert.Error(t, getErr, "no pod exists")

			// Pods elsewhere are created as usual, and githubgate keeps running.
			c.startInterceptPod(t, interceptPod{name: "elsewhere", namespace: interceptNamespace})
			c.kubectl(t, "", "rollout", "status", "-n", gateNamespace, "deployment/githubgate", "--timeout=1m")
		})

		t.Run("every container of a pod with several init containers is intercepted", func(t *testing.T) {
			t.Parallel()

			// --fail-with-body keeps GitHub's answer in the log, so a failure names
			// which side refused the request.
			request := `curl -sS --fail-with-body -w '\nstatus=%{http_code}' https://api.github.com/repos/acme/project`
			step := func(name string) interceptContainer {
				return interceptContainer{name: name, script: request + "; echo; sleep infinity"}
			}
			initStep := func(name string) interceptContainer {
				return interceptContainer{name: name, script: request}
			}

			// The init containers must succeed for the pod to start, which they can
			// only do through the interceptor.
			c.startInterceptPod(t, interceptPod{
				name: "multi", namespace: interceptNamespace,
				initContainers: []interceptContainer{initStep("first-init"), initStep("second-init")},
				containers:     []interceptContainer{step("one"), step("two")},
			})

			names := c.initContainerNames(t, interceptNamespace, "multi")
			assert.Equal(t, []string{"githubgate-intercept", "first-init", "second-init"}, names)

			for _, container := range []string{"first-init", "second-init", "one", "two"} {
				// A pod can be ready before its main containers' requests finish,
				// so wait until the request has printed its status line.
				var logs string

				eventually(t, "container "+container+" finishes its request", func() bool {
					out, err := c.tryKubectl(ctx, "", "logs", "-n", interceptNamespace, "multi", "-c", container)
					logs = out

					return err == nil && strings.Contains(out, "status=")
				}, 2*time.Minute, time.Second)

				if !assert.Contains(t, logs, "status=200", "container %s made its request through the interceptor", container) {
					// The refusal's source is in the proxy's or the fake's log.
					t.Logf("githubgate log:\n%s", c.kubectl(t, "", "logs", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "--all-containers", "--tail=50", "--prefix"))
					t.Logf("fake GitHub log:\n%s", c.kubectl(t, "", "logs", "-n", "fake", "deploy/fakegithub", "--tail=50"))
				}
			}
		})

		t.Run("a container whose proxy settings come from envFrom keeps its NO_PROXY and gains the GitHub hosts", func(t *testing.T) {
			t.Parallel()

			c.apply(t, fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: {name: proxy-settings, namespace: %s}
data:
  HTTPS_PROXY: http://egress.example:3128
  NO_PROXY: internal.example
`, interceptNamespace))

			c.startInterceptPod(t, interceptPod{
				name: "envfrom", namespace: interceptNamespace,
				containers: []interceptContainer{{
					name: "main", script: "sleep infinity",
					extra: "      envFrom:\n        - configMapRef: {name: proxy-settings}\n",
				}},
			})

			out, err := c.exec(ctx, interceptNamespace, "envfrom", "printenv NO_PROXY")
			require.NoError(t, err, out)
			assert.Equal(t, "internal.example,api.github.com,uploads.github.com,github.com", strings.TrimSpace(out))
		})
	})

	t.Run("no key material appears in the trust directory, the pod specs, the events or the logs", func(t *testing.T) {
		out, err := c.exec(ctx, interceptNamespace, "plain", "ls -A "+trustDirectory)
		require.NoError(t, err, out)
		assert.ElementsMatch(t, []string{"bundle.crt", "ca.crt", "ready"}, strings.Fields(out))

		for _, file := range strings.Fields(out) {
			content, err := c.exec(ctx, interceptNamespace, "plain", "cat "+trustDirectory+"/"+file)
			require.NoError(t, err, content)

			rest := []byte(content)
			blocks := 0

			for {
				var block *pem.Block

				block, rest = pem.Decode(rest)
				if block == nil {
					break
				}

				blocks++

				assert.Equal(t, "CERTIFICATE", block.Type, "%s holds a %s block", file, block.Type)
			}

			assert.Empty(t, strings.TrimSpace(string(rest)), "%s holds something besides certificates", file)

			if file != "ready" {
				assert.Positive(t, blocks, "%s holds certificates", file)
			}
		}

		// The sidecar's environment and arguments hold no key.
		sidecar := c.kubectl(t, "", "get", "pod", "-n", interceptNamespace, "plain", "-o", `jsonpath={.spec.initContainers[?(@.name=="githubgate-intercept")]}`)
		require.Contains(t, sidecar, "GITHUBGATE_INTERCEPT_PROXY_CA", "the sidecar was found")
		assert.False(t, strings.Contains(sidecar, "PRIVATE KEY"), "the sidecar's spec holds key material")

		corpus := c.kubectl(t, "", "get", "pods", "-n", interceptNamespace, "-o", "yaml")
		corpus += c.kubectl(t, "", "get", "events", "-A", "-o", "yaml")
		corpus += c.kubectl(t, "", "logs", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "--all-containers", "--tail=-1")

		for _, pod := range strings.Fields(c.kubectl(t, "", "get", "pods", "-n", interceptNamespace, "-o", "jsonpath={.items[*].metadata.name}")) {
			for _, container := range strings.Fields(c.kubectl(t, "", "get", "pod", "-n", interceptNamespace, pod, "-o", "jsonpath={.spec.initContainers[*].name} {.spec.containers[*].name}")) {
				logs, _ := c.tryKubectl(ctx, "", "logs", "-n", interceptNamespace, pod, "-c", container, "--tail=-1")
				corpus += logs
			}
		}

		require.Contains(t, corpus, "githubgate-intercept", "the search read the pods")
		assert.False(t, strings.Contains(corpus, "PRIVATE KEY"), "key material appears in a pod, event or log")
	})
}
