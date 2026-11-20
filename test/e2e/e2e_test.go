//go:build e2e

// Package e2e runs githubgate on a kind cluster: the chart with three
// replicas, cert-manager, the fake GitHub in the cluster, and pods that run
// the real client and real git as the ServiceAccounts of a policy.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

// slicesCompactSorted returns the distinct values of items.
func slicesCompactSorted(items []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(items)))
}

// Pinned images of cert-manager, which the chart needs for its certificates.
// Each is the repository, the tag and the digest.
const (
	certManagerChartVersion = "1.21.3"
	certManagerController   = "quay.io/jetstack/cert-manager-controller:v1.21.3@sha256:9f4f96a33c7774cef24466fcd85031997531a03a8d117f151126baa8d1290dcc"
	certManagerCAInjector   = "quay.io/jetstack/cert-manager-cainjector:v1.21.3@sha256:083d0e906f8eb44e255fe4ff817ae54e49f9ed82e22f1011bb4e9e4f4b8a641f"
	certManagerWebhook      = "quay.io/jetstack/cert-manager-webhook:v1.21.3@sha256:055ac8e9de6a0ba6763794838fe38944acc650f68ebe1b01dd8be079e580f458"
	certManagerStartupCheck = "quay.io/jetstack/cert-manager-startupapicheck:v1.21.3@sha256:1c519d4afca1413d1e4ccd78bdc756184418ab55a34569a3b3de39d32f9974e1"
)

const (
	appID           = 4242
	owner           = "acme"
	repository      = "project"
	otherRepository = "private"
	clientBinary    = "/opt/client/githubgate-client"
	fakeAPI         = "http://fakegithub.fake.svc:8080"
	gateNamespace   = "githubgate"
	proxyIP         = "10.96.0.50" // pinned cluster IP of the githubgate-proxy Service, inside kind's default service range
	callerNamespace = "ci"
)

type cluster struct {
	context     string
	serverImage string
	clientImage string
	fakeImage   string
}

func clusterFromEnv(t *testing.T) cluster {
	t.Helper()

	c := cluster{
		context:     os.Getenv("E2E_KUBE_CONTEXT"),
		serverImage: os.Getenv("E2E_SERVER_IMAGE"),
		clientImage: os.Getenv("E2E_CLIENT_IMAGE"),
		fakeImage:   os.Getenv("E2E_FAKE_IMAGE"),
	}
	if c.context == "" || c.fakeImage == "" {
		t.Skip("E2E_KUBE_CONTEXT and the image names are not set; run make test-e2e")
	}

	return c
}

// run runs a command with stdin and returns its combined output.
func run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	return out.String(), err
}

func (c cluster) kubectl(t *testing.T, stdin string, args ...string) string {
	t.Helper()

	out, err := c.tryKubectl(t.Context(), stdin, args...)
	require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), out)

	return out
}

func (c cluster) tryKubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--context", c.context}, args...)...)
}

func (c cluster) apply(t *testing.T, manifest string) {
	t.Helper()
	c.kubectl(t, manifest, "apply", "-f", "-")
}

func (c cluster) helm(t *testing.T, args ...string) {
	t.Helper()

	out, err := run(t.Context(), "", "helm", append([]string{"--kube-context", c.context}, args...)...)
	require.NoError(t, err, "helm %s: %s", strings.Join(args, " "), out)
}

// exec runs a shell script in the pod and returns its output.
func (c cluster) exec(ctx context.Context, namespace, pod, script string) (string, error) {
	return c.tryKubectl(ctx, "", "exec", "-n", namespace, pod, "--", "bash", "-c", script)
}

// control posts to the fake GitHub's control API through the API server's
// service proxy.
func (c cluster) control(t *testing.T, path string, body any) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)
	c.kubectl(t, string(raw), "create", "--raw", "/api/v1/namespaces/fake/services/fakegithub:control/proxy"+path, "-f", "-")
}

func (c cluster) controlGet(t *testing.T, path string, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(c.kubectl(t, "", "get", "--raw", "/api/v1/namespaces/fake/services/fakegithub:control/proxy"+path)), into))
}

func splitImage(ref string) (repo, digest string) {
	repo, digest, _ = strings.Cut(ref, "@")
	repo, _, _ = strings.Cut(repo, ":")

	return repo, digest
}

func (c cluster) installCertManager(t *testing.T) {
	t.Helper()

	args := []string{
		"upgrade", "--install", "cert-manager", "oci://quay.io/jetstack/charts/cert-manager",
		"--version", certManagerChartVersion, "--namespace", "cert-manager", "--create-namespace",
		"--set", "crds.enabled=true", "--wait", "--timeout", "4m",
	}

	for prefix, ref := range map[string]string{
		"image": certManagerController, "cainjector.image": certManagerCAInjector,
		"webhook.image": certManagerWebhook, "startupapicheck.image": certManagerStartupCheck,
	} {
		repo, digest := splitImage(ref)
		args = append(args, "--set", prefix+".repository="+repo, "--set", prefix+".digest="+digest)
	}

	c.helm(t, args...)
}

func (c cluster) installFakeGitHub(t *testing.T) {
	t.Helper()

	c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata: {name: fake}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakegithub, namespace: fake}
spec:
  replicas: 1
  selector: {matchLabels: {app: fakegithub}}
  template:
    metadata: {labels: {app: fakegithub}}
    spec:
      containers:
        - name: fakegithub
          image: %s
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 8080}, {containerPort: 8081}]
          readinessProbe: {tcpSocket: {port: 8080}}
---
apiVersion: v1
kind: Service
metadata: {name: fakegithub, namespace: fake}
spec:
  selector: {app: fakegithub}
  ports:
    - {name: api, port: 8080}
    - {name: control, port: 8081}
`, c.fakeImage))
	c.kubectl(t, "", "rollout", "status", "-n", "fake", "deployment/fakegithub", "--timeout=3m")
}

func publicKeyPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// installProxyCA creates the CA that signs the proxy's certificate, as an
// Issuer named proxy-ca in the githubgate namespace. Its root certificate is
// in the Secret proxy-ca-root.
func (c cluster) installProxyCA(t *testing.T) {
	t.Helper()

	c.apply(t, fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: proxy-root-bootstrap, namespace: %[1]s}
spec: {selfSigned: {}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: proxy-ca-root, namespace: %[1]s}
spec:
  isCA: true
  commonName: githubgate-e2e-proxy-ca
  secretName: proxy-ca-root
  privateKey: {algorithm: ECDSA, size: 256}
  issuerRef: {name: proxy-root-bootstrap, kind: Issuer}
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: proxy-ca, namespace: %[1]s}
spec: {ca: {secretName: proxy-ca-root}}
`, gateNamespace))
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", gateNamespace, "certificate/proxy-ca-root", "--timeout=2m")
}

func (c cluster) installGitHubgate(t *testing.T) {
	t.Helper()

	repo, tag, _ := strings.Cut(c.serverImage, ":")
	c.apply(t, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata: {name: %s}\n", gateNamespace))
	c.installProxyCA(t)
	c.helm(t, "upgrade", "--install", "githubgate", "../../deploy/charts/githubgate",
		"--namespace", gateNamespace, "--set", "image.repository="+repo, "--set", "image.tag="+tag,
		"--set", "githubBaseURL="+fakeAPI,
		"--set", "proxy.enabled=true", "--set", "proxy.ca.name=proxy-ca", "--set", "proxy.clusterIP="+proxyIP,
		"--wait", "--timeout", "4m")
}

// configure creates the App, its installation, the repository and the
// policies, and returns the private key it gave the App.
func (c cluster) configure(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	c.control(t, "/apps", map[string]any{"id": appID, "name": "e2e", "public_key_pem": publicKeyPEM(t, key)})
	c.control(t, "/installations", map[string]any{"id": 1, "app_id": appID, "owner": owner, "permissions": map[string]string{"contents": "write"}})
	c.control(t, "/repositories", map[string]any{"owner": owner, "name": repository})
	c.control(t, "/repositories", map[string]any{"owner": owner, "name": otherRepository})

	c.kubectl(t, keyPEM, "create", "secret", "generic", "app-key", "-n", gateNamespace, "--from-file=private-key.pem=/dev/stdin")
	c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata: {name: %[2]s, labels: {e2e-client: "true"}}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubApp
metadata: {name: e2e, namespace: %[1]s}
spec:
  appID: %[3]d
  privateKeySecretRef: {name: app-key, key: private-key.pem}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubInstallation
metadata: {name: acme, namespace: %[1]s}
spec:
  appRef: {name: e2e}
  owner: %[4]s
  ceiling:
    - repositories: ["*"]
      permissions: {contents: write}
  allowedNamespaces:
    selector: {matchLabels: {e2e-client: "true"}}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-build, namespace: %[2]s}
spec:
  serviceAccounts: [ci-build]
  rules:
    - name: read
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: read}
      token: {enabled: true}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-client, namespace: %[2]s}
spec:
  serviceAccounts: [ci-client]
  rules:
    - name: write
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: write}
      token: {enabled: true}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-release, namespace: %[2]s}
spec:
  serviceAccounts: [ci-release]
  rules:
    - name: write
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: write}
      token: {enabled: true}
`, gateNamespace, callerNamespace, appID, owner, repository))

	for _, kind := range []string{"githubapp/e2e", "githubinstallation/acme"} {
		c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", gateNamespace, kind, "--timeout=2m")
	}

	for _, policy := range []string{"ci-build", "ci-release", "ci-client"} {
		c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", callerNamespace, "githubaccesspolicy/"+policy, "--timeout=2m")
	}

	return keyPEM
}

// startCallerPod runs a pod as the ServiceAccount, with the client mounted as
// an image volume and the ServiceAccount token and the CA at the documented
// paths.
//
// A proxied pod also names the three GitHub hosts in hostAliases, pointing at
// the proxy, and trusts the proxy CA (only) at /var/run/secrets/githubgate-proxy/ca.crt.
func (c cluster) startCallerPod(t *testing.T, name string, proxied bool) {
	t.Helper()

	extraSpec, extraMount, extraVolume := "", "", ""
	if proxied {
		extraSpec = fmt.Sprintf("  hostAliases:\n    - ip: %s\n      hostnames: [api.github.com, uploads.github.com, github.com]\n", proxyIP)
		extraMount = "        - {name: proxy-ca, mountPath: /var/run/secrets/githubgate-proxy, readOnly: true}\n"
		extraVolume = "    - name: proxy-ca\n      configMap: {name: githubgate-proxy-ca}\n"
	}

	c.apply(t, fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: %[1]s, namespace: %[2]s}
---
apiVersion: v1
kind: Pod
metadata: {name: %[1]s, namespace: %[2]s}
spec:
%[5]s  serviceAccountName: %[1]s
  containers:
    - name: main
      image: %[3]s
      imagePullPolicy: IfNotPresent
      command: [sleep, infinity]
      env:
        - {name: GITHUBGATE_URL, value: "https://githubgate.githubgate.svc:8443"}
      volumeMounts:
        - {name: githubgate, mountPath: /var/run/secrets/githubgate, readOnly: true}
        - {name: client, mountPath: /opt/client, readOnly: true}
%[6]s  volumes:
    - name: githubgate
      projected:
        sources:
          - serviceAccountToken: {audience: githubgate, path: token, expirationSeconds: 3600}
          - configMap: {name: githubgate-ca, items: [{key: ca.crt, path: ca.crt}]}
    - name: client
      image: {reference: %[4]s, pullPolicy: IfNotPresent}
%[7]s`, name, callerNamespace, c.fakeImage, c.clientImage, extraSpec, extraMount, extraVolume))
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", callerNamespace, "pod/"+name, "--timeout=3m")
}

func TestTokenModeOnKind(t *testing.T) {
	c := clusterFromEnv(t)

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	t.Cleanup(cancel)

	c.installCertManager(t)
	c.installFakeGitHub(t)
	c.installGitHubgate(t)

	keyPEM := c.configure(t)

	// The ca ConfigMap must exist in the callers' namespace before the pods start.
	ca := c.kubectl(t, "", "get", "configmap", "githubgate-ca", "-n", gateNamespace, "-o", "jsonpath={.data.ca\\.crt}")
	c.kubectl(t, ca, "create", "configmap", "githubgate-ca", "-n", callerNamespace, "--from-file=ca.crt=/dev/stdin")

	proxyCA := c.kubectl(t, "", "get", "secret", "proxy-ca-root", "-n", gateNamespace, "-o", "jsonpath={.data.ca\\.crt}")
	proxyCAPEM, err := base64.StdEncoding.DecodeString(proxyCA)
	require.NoError(t, err)
	c.kubectl(t, string(proxyCAPEM), "create", "configmap", "githubgate-proxy-ca", "-n", callerNamespace, "--from-file=ca.crt=/dev/stdin")

	for _, name := range []string{"ci-build", "ci-release", "ci-client"} {
		c.startCallerPod(t, name, false)
	}

	c.startCallerPod(t, "ci-proxied", true)

	// Output of every failed or refused call, for the leak search.
	var clientOutput []string

	t.Run("a pull request ServiceAccount can clone but not release and the release ServiceAccount can", func(t *testing.T) {
		clone := fmt.Sprintf(`set -e
token=$(%s token --repo %s)
rm -rf /tmp/clone
git clone -q "http://x-access-token:${token}@fakegithub.fake.svc:8080/%s/%s.git" /tmp/clone 2>/dev/null
curl -s -o /dev/null -w '%%{http_code}' -X POST -H "Authorization: token ${token}" -d '{"tag_name":"v1"}' %s/repos/%s/%s/releases`,
			clientBinary, repository, owner, repository, fakeAPI, owner, repository)

		out, err := c.exec(ctx, callerNamespace, "ci-build", clone)
		require.NoError(t, err, out)
		assert.Equal(t, "403", strings.TrimSpace(out), "the read-only ServiceAccount must not create a release")

		out, err = c.exec(ctx, callerNamespace, "ci-release", clone)
		require.NoError(t, err, out)
		assert.Equal(t, "201", strings.TrimSpace(out), "the release ServiceAccount creates a release")

		out, err = c.exec(ctx, callerNamespace, "ci-build", fmt.Sprintf("%s token --repo %s --permission contents=write", clientBinary, repository))
		require.Error(t, err, "the read-only ServiceAccount must not get a write token")
		assert.Contains(t, out, "denied")

		clientOutput = append(clientOutput, out)
	})

	t.Run("a proxied pod reaches the proxy under the three GitHub host names and trusts only the proxy CA", func(t *testing.T) {
		for _, host := range []string{"api.github.com", "uploads.github.com", "github.com"} {
			script := fmt.Sprintf(`curl -sS --cacert /var/run/secrets/githubgate-proxy/ca.crt -o /dev/null -w '%%{http_code}' -H "Authorization: Bearer $(cat /var/run/secrets/githubgate/token)" https://%s/ 2>&1`, host)

			out, err := c.exec(ctx, callerNamespace, "ci-proxied", script)
			clientOutput = append(clientOutput, out)
			require.NoError(t, err, "TLS to %s must verify against the proxy CA: %s", host, out)
			assert.NotEqual(t, "000", strings.TrimSpace(out), "an HTTP response from the proxy for %s", host)
		}

		// The system trust store does not hold the proxy CA.
		out, err := c.exec(ctx, callerNamespace, "ci-proxied", "curl -sS -o /dev/null https://api.github.com/ 2>&1")
		clientOutput = append(clientOutput, out)
		require.Error(t, err, "the proxy's certificate must not verify without the proxy CA: %s", out)
	})

	t.Run("killing a replica during a stream of token requests fails no request beyond the one in flight", func(t *testing.T) {
		stream := fmt.Sprintf(`rm -f /tmp/stop /tmp/results /tmp/errors
while [ ! -e /tmp/stop ]; do
  if %s token --repo %s >/dev/null 2>>/tmp/errors; then echo ok >>/tmp/results; else echo fail >>/tmp/results; fi
done`, clientBinary, repository)

		streamDone := make(chan error, 1)

		go func() {
			_, err := c.exec(ctx, callerNamespace, "ci-release", stream)
			streamDone <- err
		}()

		count := func(word string) int {
			out, _ := c.exec(ctx, callerNamespace, "ci-release", fmt.Sprintf("grep -c %s /tmp/results || true", word))

			var n int

			_, _ = fmt.Sscan(strings.TrimSpace(out), &n)

			return n
		}

		require.Eventually(t, func() bool { return count("ok") >= 10 }, 2*time.Minute, time.Second)

		pods := strings.Fields(c.kubectl(t, "", "get", "pods", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "-o", "jsonpath={.items[*].metadata.name}"))
		require.Len(t, pods, 3, "the chart runs three replicas")

		c.kubectl(t, "", "delete", "pod", "-n", gateNamespace, pods[0], "--grace-period=0", "--force", "--wait=false")

		before := count("ok")

		require.Eventually(t, func() bool {
			out, _ := c.tryKubectl(ctx, "", "get", "deployment", "githubgate", "-n", gateNamespace, "-o", "jsonpath={.status.readyReplicas}")

			names, _ := c.tryKubectl(ctx, "", "get", "pods", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "-o", "jsonpath={.items[*].metadata.name}")

			return strings.TrimSpace(out) == "3" && !strings.Contains(names, pods[0])
		}, 3*time.Minute, time.Second)
		require.Eventually(t, func() bool { return count("ok") >= before+10 }, 2*time.Minute, time.Second)

		_, err := c.exec(ctx, callerNamespace, "ci-release", "touch /tmp/stop")
		require.NoError(t, err)
		require.NoError(t, <-streamDone)

		failures := count("fail")
		out, _ := c.exec(ctx, callerNamespace, "ci-release", "cat /tmp/errors")
		clientOutput = append(clientOutput, out)
		assert.LessOrEqual(t, failures, 1, "at most the request in flight may fail; errors: %s", out)
	})

	// This runs before the git subtests: the ServiceAccount has no cached token
	// on any of the three replicas yet, so every token the gh calls get is minted
	// with the short lifetime.
	t.Run("gh calls succeed with a fresh token each time after the token's lifetime has passed", func(t *testing.T) {
		const secondsLeft = 60 // below the server's cache minimum, so a cached token is never reused

		stub := `set -e
mkdir -p /tmp/bin
cat >/tmp/bin/gh <<'STUB'
#!/bin/bash
echo "$GH_TOKEN" >>/tmp/gh-tokens
exit 0
STUB
chmod +x /tmp/bin/gh
rm -f /tmp/gh-tokens
`
		out, err := c.exec(ctx, callerNamespace, "ci-client", stub)
		require.NoError(t, err, out)

		var before []fakegithub.MintEntry

		c.controlGet(t, "/mint-log", &before)

		// GitHub's tokens always live one hour. The server's cache runs on the real
		// clock, so moving the fake's clock cannot make a token look old without the
		// fake refusing the server's own App JWTs; a short lifetime stands in for
		// "one hour has passed".
		c.control(t, "/token-lifetime", map[string]any{"seconds": secondsLeft})
		t.Cleanup(func() {
			_, _ = c.tryKubectl(ctx, `{"seconds":0}`, "create", "--raw", "/api/v1/namespaces/fake/services/fakegithub:control/proxy/token-lifetime", "-f", "-")
		})

		gh := func(args string) {
			out, err := c.exec(ctx, callerNamespace, "ci-client", fmt.Sprintf("PATH=/tmp/bin:$PATH %s gh --repo %s/%s -- %s", clientBinary, owner, repository, args))
			require.NoError(t, err, out)
		}

		for range 2 {
			gh("pr create --title t --body b")
			gh("issue comment 1 --body hi")
		}

		out, err = c.exec(ctx, callerNamespace, "ci-client", "cat /tmp/gh-tokens")
		require.NoError(t, err, out)

		tokens := strings.Fields(out)
		require.Len(t, tokens, 4, "every call reached gh")
		assert.Len(t, slicesCompactSorted(tokens), 4, "each call after the lifetime ran out got a different token")

		var after []fakegithub.MintEntry

		c.controlGet(t, "/mint-log", &after)
		assert.GreaterOrEqual(t, len(after)-len(before), 4, "the fake minted a token for every call")

		var issued []fakegithub.IssuedToken

		c.controlGet(t, "/tokens", &issued)

		known := map[string]bool{}
		for _, token := range issued[len(before):] {
			known[token.Token] = true
		}

		for _, token := range tokens {
			assert.True(t, known[token], "the token was minted after the lifetime was set")
		}
	})

	// The client's git credential helper serves https://github.com, and the
	// fake listens on plain http. git rewrites the URL before it asks for
	// credentials, so a one-line wrapper maps the fake's host back to github.com
	// for the real helper. Everything else is the documented setup.
	gitSetup := fmt.Sprintf(`set -e
cat >/tmp/credential-wrapper <<'WRAPPER'
#!/bin/bash
sed -e 's|^protocol=http$|protocol=https|' -e 's|^host=fakegithub.fake.svc:8080$|host=github.com|' | %[1]s git-credential "$@"
WRAPPER
chmod +x /tmp/credential-wrapper
%[1]s configure
git config --global url."http://fakegithub.fake.svc:8080/".insteadOf https://github.com/
git config --global credential."http://fakegithub.fake.svc:8080".helper '!/tmp/credential-wrapper'
git config --global credential."http://fakegithub.fake.svc:8080".useHttpPath true
git config --global user.name e2e
git config --global user.email e2e@example.com
export GIT_TERMINAL_PROMPT=0
`, clientBinary)

	t.Run("git clone, fetch and push of an allowed repository work after configure", func(t *testing.T) {
		script := gitSetup + fmt.Sprintf(`rm -rf /tmp/one /tmp/two
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/one
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/two
cd /tmp/one
git commit -q --allow-empty -m pushed-by-e2e
git push -q origin HEAD:refs/heads/e2e-push
sha=$(git rev-parse HEAD)
cd /tmp/two
git fetch -q origin
test "$(git rev-parse origin/e2e-push)" = "$sha"
git ls-remote https://github.com/%[1]s/%[2]s.git refs/heads/e2e-push | grep "^$sha"
`, owner, repository)

		out, err := c.exec(ctx, callerNamespace, "ci-client", script)
		require.NoError(t, err, out)
		clientOutput = append(clientOutput, out)
	})

	t.Run("git against a repository the policy does not allow fails and the client prints the reason", func(t *testing.T) {
		script := gitSetup + fmt.Sprintf(`rm -rf /tmp/denied
git clone https://github.com/%s/%s.git /tmp/denied 2>&1
`, owner, otherRepository)

		out, err := c.exec(ctx, callerNamespace, "ci-client", script)
		require.Error(t, err, "the clone of a repository outside the policy must fail; output: %s", out)
		clientOutput = append(clientOutput, out)
		assert.Contains(t, out, "githubgate-client:", "the client prints its reason")
		assert.Contains(t, out, otherRepository, "the reason names the repository")
		assert.Contains(t, out, "fatal:", "git reports its own failure")
	})

	t.Run("no key or token appears in logs, events, status, metrics or errors", func(t *testing.T) {
		var tokens []struct {
			Token string `json:"token"`
		}

		var jwts []string

		c.controlGet(t, "/tokens", &tokens)
		c.controlGet(t, "/jwts", &jwts)
		require.NotEmpty(t, tokens, "the suite must have minted tokens")
		require.NotEmpty(t, jwts, "the suite must have sent App JWTs")

		secrets := jwts

		for _, token := range tokens {
			secrets = append(secrets, token.Token)
		}

		for _, pod := range []string{"ci-build", "ci-release", "ci-client", "ci-proxied"} {
			secrets = append(secrets, strings.TrimSpace(c.kubectl(t, "", "exec", "-n", callerNamespace, pod, "--", "cat", "/var/run/secrets/githubgate/token")))
		}

		for line := range strings.SplitSeq(keyPEM, "\n") {
			if len(line) >= 40 && !strings.HasPrefix(line, "-----") {
				secrets = append(secrets, line)
			}
		}

		corpus := strings.Join(clientOutput, "\n")
		corpus += c.kubectl(t, "", "logs", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "--all-containers", "--prefix", "--tail=-1")
		corpus += c.kubectl(t, "", "logs", "-n", "fake", "deploy/fakegithub", "--tail=-1")
		corpus += c.kubectl(t, "", "get", "events", "-A", "-o", "yaml")
		corpus += c.kubectl(t, "", "get", "githubapps,githubinstallations,githubaccesspolicies", "-A", "-o", "yaml")

		for _, pod := range strings.Fields(c.kubectl(t, "", "get", "pods", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "-o", "jsonpath={.items[*].metadata.name}")) {
			corpus += c.kubectl(t, "", "get", "--raw", fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:8080/proxy/metrics", gateNamespace, pod))
		}

		// The search is only meaningful if it reads what the server wrote.
		require.Contains(t, corpus, "githubgate_requests_total", "the metrics are collected")
		require.Contains(t, corpus, "ci-build", "the logs and status are collected")

		for _, secret := range secrets {
			require.NotEmpty(t, secret)
			assert.NotContains(t, corpus, secret, "a secret of %d characters leaked", len(secret))
		}
	})
}
