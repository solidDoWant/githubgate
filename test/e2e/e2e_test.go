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
	"path"
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
	fakeTLSIP       = "10.96.0.51" // pinned cluster IP of the fake's HTTPS Service, which githubgate's pods know as the GitHub hosts
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

	start := time.Now()
	out, err := c.tryKubectl(t.Context(), stdin, args...)

	recordWait(commandLabel("kubectl", args), time.Since(start))
	require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), out)

	return out
}

func (c cluster) tryKubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--context", c.context}, args...)...)
}

func (c cluster) apply(t *testing.T, manifest string) {
	t.Helper()

	start := time.Now()
	out, err := retryTransient(t.Context(), func() (string, error) {
		return c.tryKubectl(t.Context(), manifest, "apply", "-f", "-")
	})

	recordWait("kubectl apply", time.Since(start))
	require.NoError(t, err, "kubectl apply: %s", out)
}

func (c cluster) helm(t *testing.T, args ...string) {
	t.Helper()

	start := time.Now()
	out, err := retryTransient(t.Context(), func() (string, error) {
		return run(t.Context(), "", "helm", append([]string{"--kube-context", c.context}, args...)...)
	})

	recordWait(helmLabel(args), time.Since(start))
	require.NoError(t, err, "helm %s: %s", strings.Join(args, " "), out)
}

// transientMarkers are the messages of server-side failures that a loaded
// API server or etcd returns and that go away on their own. A rejection by
// validation or admission never matches.
var transientMarkers = []string{
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"etcdserver: no leader",
	"Internal error occurred",
	"context deadline exceeded",
	"Client.Timeout exceeded",
	"net/http: TLS handshake timeout",
	"i/o timeout",
	"connection refused",
	"connection reset by peer",
	"(InternalError)",
	"(ServiceUnavailable)",
	"(Timeout)",
	"(TooManyRequests)",
	"the server is currently unable to handle the request",
}

func isTransientError(output string) bool {
	return slices.ContainsFunc(transientMarkers, func(marker string) bool {
		return strings.Contains(output, marker)
	})
}

// retryTransient runs an idempotent command again while it fails with a
// transient server-side error, up to a few attempts.
func retryTransient(ctx context.Context, attempt func() (string, error)) (string, error) {
	const attempts = 5

	var (
		out string
		err error
	)

	for try := 1; try <= attempts; try++ {
		out, err = attempt()
		if err == nil || !isTransientError(out) || try == attempts {
			return out, err
		}

		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(time.Duration(try) * 2 * time.Second):
		}
	}

	return out, err
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
          image: %[1]s
          imagePullPolicy: IfNotPresent
          args: [--tls-address=:8443, --tls-ca-out=/tmp/fake-ca.crt]
          ports: [{containerPort: 8080}, {containerPort: 8081}, {containerPort: 8443}]
          readinessProbe: {tcpSocket: {port: 8443}}
---
apiVersion: v1
kind: Service
metadata: {name: fakegithub, namespace: fake}
spec:
  selector: {app: fakegithub}
  ports:
    - {name: api, port: 8080}
    - {name: control, port: 8081}
---
apiVersion: v1
kind: Service
metadata: {name: fakegithub-tls, namespace: fake}
spec:
  clusterIP: %[2]s
  selector: {app: fakegithub}
  ports:
    - {name: https, port: 443, targetPort: 8443}
`, c.fakeImage, fakeTLSIP))
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

	// The proxy sends requests to the fake under GitHub's own host names,
	// served with the fake's test CA.
	fakeCA := c.kubectl(t, "", "exec", "-n", "fake", "deploy/fakegithub", "--", "cat", "/tmp/fake-ca.crt")
	c.kubectl(t, fakeCA, "create", "configmap", "fake-github-ca", "-n", gateNamespace, "--from-file=ca.crt=/dev/stdin")

	c.helm(t, "upgrade", "--install", "githubgate", "../../deploy/charts/githubgate",
		"--namespace", gateNamespace, "--set", "image.repository="+repo, "--set", "image.tag="+tag,
		"--set", "githubBaseURL="+fakeAPI,
		"--set", "proxy.enabled=true", "--set", "proxy.ca.name=proxy-ca", "--set", "proxy.clusterIP="+proxyIP,
		"--set", "proxy.upstreams.caConfigMap=fake-github-ca",
		"--set", "hostAliases[0].ip="+fakeTLSIP,
		"--set", "hostAliases[0].hostnames={api.github.com,uploads.github.com,github.com}",
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
	c.control(t, "/installations", map[string]any{"id": 1, "app_id": appID, "owner": owner, "permissions": map[string]string{
		"contents": "write", "issues": "write", "pull_requests": "write",
	}})
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
      permissions: {contents: write, issues: write, pull_requests: write}
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
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-proxied, namespace: %[2]s}
spec:
  serviceAccounts: [ci-proxied]
  rules:
    - name: proxy
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: write, issues: write, pull_requests: write}
      proxy:
        enabled: true
        denyOperations: [repos/create-release]
        denyMutations: [mergePullRequest]
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-proxied-limited, namespace: %[2]s}
spec:
  serviceAccounts: [ci-proxied-limited]
  rules:
    - name: proxy
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: write, issues: write}
      proxy:
        enabled: true
        allowOperations: [repos/get]
        pushRefs: [refs/heads/agent/**]
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: ci-proxied-push, namespace: %[2]s}
spec:
  serviceAccounts: [ci-proxied-push]
  rules:
    - name: proxy
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[5]s]
      permissions: {contents: write}
      proxy:
        enabled: true
        pushRefs: [refs/heads/agent/**]
`, gateNamespace, callerNamespace, appID, owner, repository))

	for _, kind := range []string{"githubapp/e2e", "githubinstallation/acme"} {
		c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", gateNamespace, kind, "--timeout=2m")
	}

	for _, policy := range []string{"ci-build", "ci-release", "ci-client", "ci-proxied", "ci-proxied-limited", "ci-proxied-push"} {
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
		// GitHub's archive storage host is not proxied: it reaches the fake directly.
		extraSpec = fmt.Sprintf("  hostAliases:\n    - ip: %s\n      hostnames: [api.github.com, uploads.github.com, github.com]\n    - ip: %s\n      hostnames: [%s]\n",
			proxyIP, fakeTLSIP, fakegithub.StorageHost)
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
	c.startCallerPod(t, "ci-proxied-limited", true)
	c.startCallerPod(t, "ci-proxied-push", true)

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

	// killDuringStream runs request in a loop in pod, force-deletes one of
	// the three replicas, and expects at most the request in flight to fail.
	killDuringStream := func(t *testing.T, pod, request string) {
		t.Helper()

		stream := fmt.Sprintf(`rm -f /tmp/stop /tmp/results /tmp/errors
while [ ! -e /tmp/stop ]; do
  if %s >/dev/null 2>>/tmp/errors; then echo ok >>/tmp/results; else echo fail >>/tmp/results; fi
done`, request)

		streamDone := make(chan error, 1)

		go func() {
			_, err := c.exec(ctx, callerNamespace, pod, stream)
			streamDone <- err
		}()

		count := func(word string) int {
			out, _ := c.exec(ctx, callerNamespace, pod, fmt.Sprintf("grep -c %s /tmp/results || true", word))

			var n int

			_, _ = fmt.Sscan(strings.TrimSpace(out), &n)

			return n
		}

		eventually(t, "ten tokens served", func() bool { return count("ok") >= 10 }, 2*time.Minute, time.Second)

		pods := strings.Fields(c.kubectl(t, "", "get", "pods", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "-o", "jsonpath={.items[*].metadata.name}"))
		require.Len(t, pods, 3, "the chart runs three replicas")

		c.kubectl(t, "", "delete", "pod", "-n", gateNamespace, pods[0], "--grace-period=0", "--force", "--wait=false")

		before := count("ok")

		eventually(t, "replacement pod ready", func() bool {
			// Read the pods themselves: the deployment's status can lag behind the deletion.
			out, _ := c.tryKubectl(ctx, "", "get", "pods", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate",
				"-o", `jsonpath={range .items[*]}{.metadata.name}={.metadata.deletionTimestamp}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)

			lines := strings.Fields(out)
			if len(lines) != 3 {
				return false
			}

			for _, line := range lines {
				if strings.HasPrefix(line, pods[0]+"=") || !strings.HasSuffix(line, "==True") {
					return false
				}
			}

			return true
		}, 3*time.Minute, time.Second)
		eventually(t, "ten tokens served after pod deletion", func() bool { return count("ok") >= before+10 }, 2*time.Minute, time.Second)

		_, err := c.exec(ctx, callerNamespace, pod, "touch /tmp/stop")
		require.NoError(t, err)
		require.NoError(t, <-streamDone)

		failures := count("fail")
		out, _ := c.exec(ctx, callerNamespace, pod, "cat /tmp/errors")
		clientOutput = append(clientOutput, out)
		assert.LessOrEqual(t, failures, 1, "at most the request in flight may fail; errors: %s", out)
	}

	t.Run("killing a replica during a stream of token requests fails no request beyond the one in flight", func(t *testing.T) {
		killDuringStream(t, "ci-release", fmt.Sprintf("%s token --repo %s", clientBinary, repository))
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

	// proxied runs a script in a proxied pod with SA_TOKEN set to its
	// ServiceAccount token, which it sends where a GitHub token would go.
	proxied := func(t *testing.T, pod, script string) string {
		t.Helper()

		out, err := c.exec(ctx, callerNamespace, pod, `set -e
export SA_TOKEN=$(cat /var/run/secrets/githubgate/token)
export PROXY_CA=/var/run/secrets/githubgate-proxy/ca.crt
`+script)
		clientOutput = append(clientOutput, out)
		require.NoError(t, err, out)

		return out
	}

	// call makes one request through the proxy and prints the status, the
	// headers and the body, so the leak search reads all of them.
	call := func(method, target, body string) string {
		data := ""
		if body != "" {
			data = fmt.Sprintf("-H 'Content-Type: application/json' --data '%s'", body)
		}

		return fmt.Sprintf(`curl -sS -i --cacert "$PROXY_CA" -w '\nstatus=%%{http_code}\n' -X %s -H "Authorization: token $SA_TOKEN" -H 'Accept: application/vnd.github+json' %s '%s'
`, method, data, target)
	}

	t.Run("a proxied pod reads an allowed repository with gh api", func(t *testing.T) {
		out := proxied(t, "ci-proxied", fmt.Sprintf(`GH_TOKEN="$SA_TOKEN" SSL_CERT_FILE="$PROXY_CA" gh api repos/%s/%s --jq .name`, owner, repository))

		assert.Equal(t, repository, strings.TrimSpace(out))
	})

	t.Run("a repository the proxy rules don't cover is refused naming it and never minted for", func(t *testing.T) {
		var before []fakegithub.MintEntry

		c.controlGet(t, "/mint-log", &before)

		out := proxied(t, "ci-proxied", call("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, otherRepository), ""))

		assert.Contains(t, out, "status=403")
		assert.Contains(t, out, fmt.Sprintf("%s/%s", owner, otherRepository))

		var after []fakegithub.MintEntry

		c.controlGet(t, "/mint-log", &after)
		assert.Len(t, after, len(before), "a token was minted for a refused request")
	})

	t.Run("a denied operation is refused naming it while another operation on the same repository works", func(t *testing.T) {
		out := proxied(t, "ci-proxied", call("POST", fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", owner, repository), `{"tag_name":"proxy-v1"}`))
		assert.Contains(t, out, "status=403")
		assert.Contains(t, out, "repos/create-release")

		out = proxied(t, "ci-proxied", call("POST", fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/1/comments", owner, repository), `{"body":"hi"}`))
		assert.Contains(t, out, "status=201")
	})

	t.Run("with allowOperations only the listed operations work", func(t *testing.T) {
		out := proxied(t, "ci-proxied-limited", call("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repository), ""))
		assert.Contains(t, out, "status=200")

		out = proxied(t, "ci-proxied-limited", call("POST", fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/1/comments", owner, repository), `{"body":"hi"}`))
		assert.Contains(t, out, "status=403")
		assert.Contains(t, out, "issues/create-comment")
	})

	t.Run("GraphQL queries work, a denied mutation is refused naming it and createPullRequest works", func(t *testing.T) {
		// The pull request needs a base and a head branch.
		setup := gitSetup + fmt.Sprintf(`rm -rf /tmp/pr
git clone -q https://github.com/%[1]s/%[2]s.git /tmp/pr 2>/dev/null
cd /tmp/pr
git commit -q --allow-empty -m base
git push -q origin HEAD:refs/heads/main
git commit -q --allow-empty -m head
git push -q origin HEAD:refs/heads/proxy-pr
`, owner, repository)
		out, err := c.exec(ctx, callerNamespace, "ci-client", setup)
		require.NoError(t, err, out)

		gh := `GH_TOKEN="$SA_TOKEN" SSL_CERT_FILE="$PROXY_CA" gh api graphql `

		out = proxied(t, "ci-proxied", gh+fmt.Sprintf(`-f query='query { repository(owner: "%s", name: "%s") { id } }' --jq .data.repository.id`, owner, repository))
		repositoryID := strings.TrimSpace(out)
		require.NotEmpty(t, repositoryID)

		out = proxied(t, "ci-proxied", gh+fmt.Sprintf(`-f query='mutation { createPullRequest(input: {repositoryId: "%s", baseRefName: "main", headRefName: "proxy-pr", title: "t"}) { pullRequest { number } } }' --jq .data.createPullRequest.pullRequest.number`, repositoryID))
		assert.NotEmpty(t, strings.TrimSpace(out), "the pull request was created")

		out = proxied(t, "ci-proxied", call("POST", "https://api.github.com/graphql", `{"query":"mutation { mergePullRequest(input: {pullRequestId: \"x\"}) { clientMutationId } }"}`))
		assert.Contains(t, out, "status=403")
		assert.Contains(t, out, "mergePullRequest")
	})

	t.Run("a release asset upload follows the same operation rules as the API", func(t *testing.T) {
		out, err := c.exec(ctx, callerNamespace, "ci-release", fmt.Sprintf(`set -e
token=$(%s token --repo %s)
curl -sS -X POST -H "Authorization: token ${token}" -d '{"tag_name":"asset-v1"}' %s/repos/%s/%s/releases | sed -n 's/.*"id": *\([0-9]*\).*/\1/p'`,
			clientBinary, repository, fakeAPI, owner, repository))
		require.NoError(t, err, out)

		releaseID := strings.TrimSpace(out)
		require.NotEmpty(t, releaseID)

		upload := fmt.Sprintf(`curl -sS -i --cacert "$PROXY_CA" -X POST -H "Authorization: token $SA_TOKEN" -H 'Content-Type: text/plain' -w '\nstatus=%%{http_code}\n' --data-binary asset 'https://uploads.github.com/repos/%s/%s/releases/%s/assets?name=a.txt'
`, owner, repository, releaseID)

		out = proxied(t, "ci-proxied", upload)
		assert.Contains(t, out, "status=201")

		out = proxied(t, "ci-proxied-limited", upload)
		assert.Contains(t, out, "status=403")
		assert.Contains(t, out, "repos/upload-release-asset")
	})

	t.Run("with pushRefs a push to an allowed branch works and a push to master or a tag is refused naming the ref", func(t *testing.T) {
		// git sends the ServiceAccount token as the Basic password, which is
		// what a credential helper would do.
		script := fmt.Sprintf(`remote=https://github.com/%[1]s/%[2]s.git
header="Authorization: Basic $(printf 'x-access-token:%%s' "$SA_TOKEN" | base64 | tr -d '\n')"
pushgit() { git -c http.sslCAInfo="$PROXY_CA" -c http.extraHeader="$header" "$@"; }
rm -rf /tmp/push-refs
git init -q /tmp/push-refs
cd /tmp/push-refs
git -c user.name=e2e -c user.email=e2e@example.com commit -q --allow-empty -m push-refs
sha=$(git rev-parse HEAD)
pushgit push -q "$remote" HEAD:refs/heads/agent/x
pushgit ls-remote "$remote" refs/heads/agent/x | grep -q "^$sha"
for ref in refs/heads/master refs/tags/push-refs-v1; do
  if pushgit push --force "$remote" HEAD:$ref 2>/tmp/push-refs.err; then echo "pushed $ref"; exit 1; fi
  cat /tmp/push-refs.err
  if pushgit ls-remote "$remote" $ref | grep -q "^$sha"; then echo "$ref reached GitHub"; exit 1; fi
done
`, owner, repository)

		out := proxied(t, "ci-proxied-limited", script)

		for _, ref := range []string{"refs/heads/master", "refs/tags/push-refs-v1"} {
			assert.Contains(t, out, fmt.Sprintf("githubgate: repository %q: ref %q is outside pushRefs", owner+"/"+repository, ref))
		}
	})

	// proxyGitSetup is the documented setup of git in a proxied pod.
	proxyGitSetup := fmt.Sprintf(`export PATH=%s:$PATH GIT_TERMINAL_PROMPT=0
githubgate-client configure --proxy >/dev/null
git config --global user.name e2e
git config --global user.email e2e@example.com
`, path.Dir(clientBinary))

	t.Run("git clone and fetch of an allowed repository work from a proxied pod after configure --proxy", func(t *testing.T) {
		proxied(t, "ci-proxied", proxyGitSetup+fmt.Sprintf(`rm -rf /tmp/proxy-clone
git clone -q https://github.com/%s/%s.git /tmp/proxy-clone
`, owner, repository))

		// A commit pushed by someone else, which the fetch must bring.
		out, err := c.exec(ctx, callerNamespace, "ci-client", gitSetup+fmt.Sprintf(`rm -rf /tmp/fetch-source
git clone -q https://github.com/%s/%s.git /tmp/fetch-source
cd /tmp/fetch-source
git commit -q --allow-empty -m fetched-through-the-proxy
git push -q origin HEAD:refs/heads/proxy-fetch
git rev-parse HEAD
`, owner, repository))
		require.NoError(t, err, out)

		lines := strings.Split(strings.TrimSpace(out), "\n")
		sha := lines[len(lines)-1]

		out = proxied(t, "ci-proxied", proxyGitSetup+`cd /tmp/proxy-clone
git fetch -q origin
git rev-parse origin/proxy-fetch
`)
		assert.Equal(t, sha, strings.TrimSpace(out))
	})

	t.Run("push through the proxy publishes a branch inside pushRefs and refuses one outside it", func(t *testing.T) {
		out := proxied(t, "ci-proxied-push", proxyGitSetup+fmt.Sprintf(`rm -rf /tmp/proxy-push
git clone -q https://github.com/%s/%s.git /tmp/proxy-push
cd /tmp/proxy-push
git checkout -q -b agent/signed
echo signed >signed.txt
git add signed.txt
git commit -q -m "signed through the proxy"
githubgate-client push --proxy
test "$(git ls-remote origin refs/heads/agent/signed | cut -f1)" = "$(git rev-parse HEAD)"
git checkout -q -b outside
echo outside >outside.txt
git add outside.txt
git commit -q -m "outside pushRefs"
if githubgate-client push --proxy 2>&1; then echo "published outside pushRefs"; exit 1; fi
if [ -n "$(git ls-remote origin refs/heads/outside)" ]; then echo "outside reached GitHub"; exit 1; fi
`, owner, repository))

		assert.Contains(t, out, "published 1 commit(s) to agent/signed")
		assert.Contains(t, out, `ref "refs/heads/outside" is outside pushRefs`)
	})

	t.Run("killing a replica during a stream of proxied requests fails no request beyond the one in flight", func(t *testing.T) {
		killDuringStream(t, "ci-proxied", fmt.Sprintf(
			`curl -sSf --cacert /var/run/secrets/githubgate-proxy/ca.crt -H "Authorization: token $(cat /var/run/secrets/githubgate/token)" https://api.github.com/repos/%s/%s`,
			owner, repository))
	})

	t.Run("downloading a repository archive through its redirect to GitHub's storage works from a proxied pod", func(t *testing.T) {
		// The storage host is GitHub itself, trusted through the system CAs
		// there; here it is the fake, trusted through its test CA.
		fakeCA := c.kubectl(t, "", "exec", "-n", "fake", "deploy/fakegithub", "--", "cat", "/tmp/fake-ca.crt")

		out := proxied(t, "ci-proxied", fmt.Sprintf(`cat "$PROXY_CA" >/tmp/ca-bundle.crt
cat >>/tmp/ca-bundle.crt <<'PEM'
%s
PEM
curl -sSfL --cacert /tmp/ca-bundle.crt -H "Authorization: token $SA_TOKEN" -o /tmp/archive.tar -w 'status=%%{http_code} redirects=%%{num_redirects}' https://api.github.com/repos/%s/%s/tarball/main
`, strings.TrimSpace(fakeCA), owner, repository))

		assert.Contains(t, out, "status=200 redirects=1")
	})

	t.Run("every proxied request writes one audit line without a token", func(t *testing.T) {
		// Its own request: a replica killed earlier took its log with it.
		out := proxied(t, "ci-proxied", call("POST", fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", owner, repository), `{"tag_name":"audit-v1"}`))
		require.Contains(t, out, "status=403")

		logs := c.kubectl(t, "", "logs", "-n", gateNamespace, "-l", "app.kubernetes.io/name=githubgate", "--all-containers", "--tail=-1")

		found := false

		for line := range strings.SplitSeq(logs, "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) != nil || entry["mode"] != "proxy" || entry["operation"] != "repos/create-release" {
				continue
			}

			found = true

			assert.Equal(t, callerNamespace, entry["namespace"])
			assert.Equal(t, "ci-proxied", entry["service_account"])
			assert.Equal(t, "refused", entry["decision"])
			assert.Equal(t, []any{owner + "/" + repository}, entry["repositories"])
			assert.Contains(t, entry, "refs")
			assert.Contains(t, entry, "mutations")
		}

		assert.True(t, found, "the refused release has an audit line")
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

		for _, pod := range []string{"ci-build", "ci-release", "ci-client", "ci-proxied", "ci-proxied-limited"} {
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
