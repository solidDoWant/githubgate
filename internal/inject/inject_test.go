package inject_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/solidDoWant/githubgate/internal/inject"
)

func certificateDER(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}}, &key.PublicKey, key)
	require.NoError(t, err)

	return der
}

const trust = "/var/run/githubgate/trust"

func writeCA(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER(t)}), 0o600))

	return path
}

func newHandler(t *testing.T) *inject.Handler {
	t.Helper()

	return inject.New(inject.Config{
		Image: "ghcr.io/example/client@sha256:abc", ProxyAddress: "githubgate-proxy.gg.svc:443",
		Audience: "githubgate", ProxyCAFile: writeCA(t),
	})
}

func review(t *testing.T, handler *inject.Handler, pod *corev1.Pod) (admission.Response, *corev1.Pod) {
	t.Helper()

	raw, err := json.Marshal(pod)
	require.NoError(t, err)

	response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}}})
	if !response.Allowed {
		return response, nil
	}

	patch, err := json.Marshal(response.Patches)
	require.NoError(t, err)

	decoded, err := jsonpatch.DecodePatch(patch)
	require.NoError(t, err)

	out, err := decoded.Apply(raw)
	require.NoError(t, err)

	var mutated corev1.Pod
	require.NoError(t, json.Unmarshal(out, &mutated))

	return response, &mutated
}

func env(container corev1.Container, name string) (string, bool) {
	for _, variable := range container.Env {
		if variable.Name == name {
			return variable.Value, true
		}
	}

	return "", false
}

func basePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init-a", Image: "ghcr.io/example/a@sha256:abc"}, {Name: "init-b", Image: "ghcr.io/example/b@sha256:abc"}},
			Containers:     []corev1.Container{{Name: "main", Image: "ghcr.io/example/m@sha256:abc"}},
		},
	}
}

func TestSkipAndReject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		change func(*corev1.Pod)
		reason string
	}{
		"hostNetwork": {func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "hostNetwork is true"},
		"hostAliases": {func(p *corev1.Pod) {
			p.Spec.HostAliases = []corev1.HostAlias{{IP: "1.2.3.4", Hostnames: []string{"x", "API.GitHub.com."}}}
		}, "hostAliases names API.GitHub.com."},
		"SSL_CERT_FILE":       {func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: "x"}} }, "unreadable:main:SSL_CERT_FILE"},
		"CURL_CA_BUNDLE init": {func(p *corev1.Pod) { p.Spec.InitContainers[1].Env = []corev1.EnvVar{{Name: "CURL_CA_BUNDLE"}} }, "unreadable:init-b:CURL_CA_BUNDLE"},
		"GIT_SSL_CAINFO":      {func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "GIT_SSL_CAINFO"}} }, "unreadable:main:GIT_SSL_CAINFO"},
		"REQUESTS_CA_BUNDLE":  {func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "REQUESTS_CA_BUNDLE"}} }, "unreadable:main:REQUESTS_CA_BUNDLE"},
		"NIX_SSL_CERT_FILE":   {func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "NIX_SSL_CERT_FILE"}} }, "unreadable:main:NIX_SSL_CERT_FILE"},
		"NODE_EXTRA_CA_CERTS": {func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "NODE_EXTRA_CA_CERTS"}} }, "unreadable:main:NODE_EXTRA_CA_CERTS"},
		"port 443":            {func(p *corev1.Pod) { p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 443}} }, "container main uses port 443"},
		"sysctl above 443": {func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "444"}}}
		}, "net.ipv4.ip_unprivileged_port_start is above 443"},
		"reserved container": {func(p *corev1.Pod) {
			p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "githubgate-intercept"})
		}, "a container is named githubgate-intercept"},
		"reserved volume": {func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: "githubgate-trust"}} }, "a volume is named githubgate-trust"},
		"reserved state":  {func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: "githubgate-intercept-state"}} }, "a volume is named githubgate-intercept-state"},
		"reserved token mount": {func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "githubgate-token"}}
		}, "a mount is named githubgate-token"},
		"listen without port":         {listenAnnotation("10.0.2.1"), `the githubgate.soliddowant.dev/listen annotation "10.0.2.1" isn't a specific IP with port 443`},
		"listen on another port":      {listenAnnotation("10.0.2.1:8443"), `the githubgate.soliddowant.dev/listen annotation "10.0.2.1:8443" isn't a specific IP with port 443`},
		"listen on 0.0.0.0":           {listenAnnotation("0.0.0.0:443"), `the githubgate.soliddowant.dev/listen annotation "0.0.0.0:443" isn't a specific IP with port 443`},
		"listen on ::":                {listenAnnotation("[::]:443"), `the githubgate.soliddowant.dev/listen annotation "[::]:443" isn't a specific IP with port 443`},
		"listen on mapped 0.0.0.0":    {listenAnnotation("[::ffff:0.0.0.0]:443"), `the githubgate.soliddowant.dev/listen annotation "[::ffff:0.0.0.0]:443" isn't a specific IP with port 443`},
		"listen on mapped ::ffff:0:0": {listenAnnotation("[::ffff:0:0]:443"), `the githubgate.soliddowant.dev/listen annotation "[::ffff:0:0]:443" isn't a specific IP with port 443`},
		"listen on a host name":       {listenAnnotation("localhost:443"), `the githubgate.soliddowant.dev/listen annotation "localhost:443" isn't a specific IP with port 443`},
		"annotation without sidecar":  {func(p *corev1.Pod) { p.Annotations = map[string]string{inject.InjectedAnnotation: "true"} }, "the injected annotation is set without the githubgate-intercept init container"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pod := basePod()
			test.change(pod)

			response, _ := review(t, newHandler(t), pod)
			require.False(t, response.Allowed)

			reason := test.reason
			if parts := strings.Split(reason, ":"); len(parts) == 3 && parts[0] == "unreadable" {
				reason = "container \"" + parts[1] + "\" sets " + parts[2] + " to a file githubgate can't read; put the CA in a ConfigMap and name it in the namespace annotation githubgate.soliddowant.dev/extra-ca-configmap"
			}

			assert.Equal(t, "githubgate: pod can't be intercepted: "+reason, response.Result.Message)
		})
	}

	t.Run("opt out is unchanged", func(t *testing.T) {
		t.Parallel()

		pod := basePod()
		pod.Annotations = map[string]string{inject.InjectAnnotation: "false"}
		pod.Spec.HostNetwork = true

		response, _ := review(t, newHandler(t), pod)
		assert.True(t, response.Allowed)
		assert.Empty(t, response.Patches)
	})

	t.Run("sysctl at 443 is kept", func(t *testing.T) {
		t.Parallel()

		pod := basePod()
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "80"}}}

		_, mutated := review(t, newHandler(t), pod)
		require.NotNil(t, mutated)
		assert.Equal(t, []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "80"}}, mutated.Spec.SecurityContext.Sysctls)
	})
}

func TestDeniesWithoutProxyCA(t *testing.T) {
	t.Parallel()

	empty := filepath.Join(t.TempDir(), "empty.crt")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))

	for name, path := range map[string]string{"missing": filepath.Join(t.TempDir(), "none"), "no certificate": empty} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handler := inject.New(inject.Config{Image: "ghcr.io/example/i@sha256:abc", ProxyCAFile: path})
			assert.False(t, handler.Ready())

			for _, optOut := range []bool{false, true} {
				pod := basePod()
				if optOut {
					pod.Annotations = map[string]string{inject.InjectAnnotation: "false"}
				}

				response, _ := review(t, handler, pod)
				require.False(t, response.Allowed)
				assert.Equal(t, "githubgate: injection isn't configured (proxy CA)", response.Result.Message)
			}
		})
	}
}

func assertMutated(t *testing.T, pod *corev1.Pod) {
	t.Helper()

	spec := pod.Spec
	require.NotEmpty(t, spec.InitContainers)
	assert.Equal(t, "githubgate-intercept", spec.InitContainers[0].Name)
	assert.Equal(t, "true", pod.Annotations[inject.InjectedAnnotation])

	sidecars := 0

	for index, container := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if container.Name == "githubgate-intercept" {
			sidecars++

			continue
		}

		require.Positive(t, index)

		value, _ := env(container, "SSL_CERT_FILE")
		assert.Equal(t, trust+"/bundle.crt", value, container.Name)
		value, _ = env(container, "NIX_SSL_CERT_FILE")
		assert.Equal(t, trust+"/bundle.crt", value, container.Name)
		value, _ = env(container, "NODE_EXTRA_CA_CERTS")
		assert.Equal(t, trust+"/ca.crt", value, container.Name)
		assert.Equal(t, []corev1.VolumeMount{{Name: "githubgate-trust", MountPath: trust, ReadOnly: true}}, container.VolumeMounts, container.Name)
	}

	assert.Equal(t, 1, sidecars)
}

func TestMutatesPod(t *testing.T) {
	t.Parallel()

	pod := basePod()
	pod.Spec.InitContainers[0].Env = []corev1.EnvVar{{Name: "HTTPS_PROXY", Value: "http://p:3128"}, {Name: "NO_PROXY", Value: "10.0.0.0/8"}}
	pod.Spec.InitContainers[1].Env = []corev1.EnvVar{{Name: "GITHUB_TOKEN", Value: "x"}}
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "http_proxy", Value: "http://p:3128"}, {Name: "GH_TOKEN", Value: "mine"}}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "plain", Image: "ghcr.io/example/p@sha256:abc"})

	_, mutated := review(t, newHandler(t), pod)
	require.NotNil(t, mutated)
	assertMutated(t, mutated)

	hosts := "api.github.com,uploads.github.com,github.com"

	assert.Equal(t, []corev1.HostAlias{{IP: "127.43.0.1", Hostnames: []string{"api.github.com", "uploads.github.com", "github.com"}}}, mutated.Spec.HostAliases)
	assert.Equal(t, []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "443"}}, mutated.Spec.SecurityContext.Sysctls)

	init0 := mutated.Spec.InitContainers[1]
	value, _ := env(init0, "NO_PROXY")
	assert.Equal(t, "10.0.0.0/8,"+hosts, value)
	value, _ = env(init0, "no_proxy")
	assert.Equal(t, hosts, value)

	main := mutated.Spec.Containers[0]
	value, _ = env(main, "NO_PROXY")
	assert.Equal(t, hosts, value)
	value, _ = env(main, "GH_TOKEN")
	assert.Equal(t, "mine", value)

	_, hasToken := env(mutated.Spec.InitContainers[2], "GH_TOKEN")
	assert.False(t, hasToken, "GITHUB_TOKEN set, so no GH_TOKEN placeholder")

	_, hasNoProxy := env(mutated.Spec.Containers[1], "NO_PROXY")
	assert.False(t, hasNoProxy, "no egress proxy variable, so no NO_PROXY")

	value, _ = env(mutated.Spec.Containers[1], "GH_TOKEN")
	assert.Equal(t, "githubgate", value)

	volumes := map[string]corev1.Volume{}
	for _, volume := range mutated.Spec.Volumes {
		volumes[volume.Name] = volume
	}

	token := volumes["githubgate-token"].Projected.Sources[0].ServiceAccountToken
	assert.Equal(t, "githubgate", token.Audience)
	assert.Equal(t, int64(3600), *token.ExpirationSeconds)
	assert.Equal(t, corev1.StorageMediumMemory, volumes["githubgate-intercept-state"].EmptyDir.Medium)
	assert.Equal(t, int64(1<<20), volumes["githubgate-intercept-state"].EmptyDir.SizeLimit.Value())
	assert.Equal(t, corev1.StorageMediumMemory, volumes["githubgate-trust"].EmptyDir.Medium)
	assert.Equal(t, int64(4<<20), volumes["githubgate-trust"].EmptyDir.SizeLimit.Value())

	sidecar := mutated.Spec.InitContainers[0]
	assert.Equal(t, "ghcr.io/example/client@sha256:abc", sidecar.Image)
	assert.Equal(t, []string{"intercept"}, sidecar.Args)
	assert.Equal(t, corev1.ContainerRestartPolicyAlways, *sidecar.RestartPolicy)
	assert.Equal(t, []string{"/githubgate-client", "intercept", "--probe"}, sidecar.StartupProbe.Exec.Command)
	assert.Equal(t, int32(1), sidecar.StartupProbe.PeriodSeconds)
	assert.Equal(t, int32(30), sidecar.StartupProbe.FailureThreshold)
	assert.Equal(t, int32(10), sidecar.StartupProbe.TimeoutSeconds)
	assert.Equal(t, int64(65532), *sidecar.SecurityContext.RunAsUser)
	assert.Equal(t, "64Mi", sidecar.Resources.Requests.Memory().String())
	assert.Equal(t, "64Mi", sidecar.Resources.Limits.Memory().String())
	assert.Equal(t, "10m", sidecar.Resources.Requests.Cpu().String())
	assert.True(t, sidecar.Resources.Limits.Cpu().IsZero())

	value, _ = env(sidecar, "GITHUBGATE_INTERCEPT_PROXY_ADDRESS")
	assert.Equal(t, "githubgate-proxy.gg.svc:443", value)
	value, _ = env(sidecar, "GITHUBGATE_INTERCEPT_PROXY_CA")
	assert.Contains(t, value, "BEGIN CERTIFICATE")

	_, hasExtra := env(sidecar, "GITHUBGATE_INTERCEPT_EXTRA_CA")
	assert.False(t, hasExtra)

	mounted := map[string]bool{}

	for _, container := range append(append([]corev1.Container{}, mutated.Spec.InitContainers[1:]...), mutated.Spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			mounted[mount.Name] = true
		}
	}

	assert.False(t, mounted["githubgate-token"])
	assert.False(t, mounted["githubgate-intercept-state"])
}

func listenAnnotation(value string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Annotations = map[string]string{inject.ListenAnnotation: value} }
}

func TestListenAnnotationSetsTheInterceptorAddressAndHostAliases(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ annotation, ip string }{
		{annotation: "10.0.2.1:443", ip: "10.0.2.1"},
		{annotation: "[fd00::1]:443", ip: "fd00::1"},
	} {
		t.Run(test.annotation, func(t *testing.T) {
			t.Parallel()

			handler := newHandler(t)
			pod := basePod()
			listenAnnotation(test.annotation)(pod)

			_, mutated := review(t, handler, pod)
			require.NotNil(t, mutated)

			assert.Equal(t, []corev1.HostAlias{{IP: test.ip, Hostnames: []string{"api.github.com", "uploads.github.com", "github.com"}}}, mutated.Spec.HostAliases)

			sidecar := mutated.Spec.InitContainers[0]
			assert.Equal(t, []string{"intercept", "--listen", test.annotation}, sidecar.Args)
			assert.Equal(t, []string{"/githubgate-client", "intercept", "--probe", "--listen", test.annotation}, sidecar.StartupProbe.Exec.Command)

			// A reinvocation replaces its own alias rather than rejecting it.
			_, twice := review(t, handler, mutated)
			require.NotNil(t, twice)
			assert.Equal(t, mutated.Spec.HostAliases, twice.Spec.HostAliases)
		})
	}
}

func TestMutationIsIdempotentOnReinvocation(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)

	_, once := review(t, handler, basePod())
	require.NotNil(t, once)

	// Another webhook puts its own container first and adds one.
	once.Spec.InitContainers = append([]corev1.Container{{Name: "other-init", Image: "ghcr.io/example/o@sha256:abc"}}, once.Spec.InitContainers...)
	once.Spec.Containers = append(once.Spec.Containers, corev1.Container{Name: "late", Image: "ghcr.io/example/l@sha256:abc"})

	_, twice := review(t, handler, once)
	require.NotNil(t, twice)
	assertMutated(t, twice)

	_, third := review(t, handler, twice)
	require.NotNil(t, third)

	twiceJSON, err := json.Marshal(twice.Spec)
	require.NoError(t, err)
	thirdJSON, err := json.Marshal(third.Spec)
	require.NoError(t, err)
	assert.JSONEq(t, string(twiceJSON), string(thirdJSON))
	assert.Len(t, twice.Spec.HostAliases, 1)
	assert.Len(t, twice.Spec.Volumes, 3)

	names := []string{}
	for _, container := range twice.Spec.InitContainers {
		names = append(names, container.Name)
	}

	assert.Equal(t, []string{"githubgate-intercept", "other-init", "init-a", "init-b"}, names)

	t.Run("reject checks run again", func(t *testing.T) {
		t.Parallel()

		bad := twice.DeepCopy()
		bad.Spec.Containers[0].Env = append(bad.Spec.Containers[0].Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/other"})

		response, _ := review(t, handler, bad)
		assert.False(t, response.Allowed)

		bad = twice.DeepCopy()
		bad.Spec.HostNetwork = true

		response, _ = review(t, handler, bad)
		assert.False(t, response.Allowed)
	})
}

func TestMutatedPodPassesRestrictedPodSecurity(t *testing.T) {
	t.Parallel()

	pod := basePod()
	yes, no := true, false
	secure := &corev1.SecurityContext{
		RunAsNonRoot: &yes, AllowPrivilegeEscalation: &no,
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	for index := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[index].SecurityContext = secure
	}

	pod.Spec.Containers[0].SecurityContext = secure
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: secure.SeccompProfile}

	_, mutated := review(t, newHandler(t), pod)
	require.NotNil(t, mutated)

	evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), nil)
	require.NoError(t, err)

	level := api.LevelVersion{Level: api.LevelRestricted, Version: api.LatestVersion()}
	for _, result := range evaluator.EvaluatePod(level, &mutated.ObjectMeta, &mutated.Spec) {
		assert.True(t, result.Allowed, result.ForbiddenReason+" "+result.ForbiddenDetail)
	}
}

func TestNoProxyKeepsEnvFromValue(t *testing.T) {
	t.Parallel()

	hosts := "api.github.com,uploads.github.com,github.com"
	extended := "$(NO_PROXY)," + hosts
	envFrom := []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "proxy-settings"}}}}

	tests := map[string]struct {
		env      []corev1.EnvVar
		envFrom  []corev1.EnvFromSource
		wantUp   string
		wantLow  string
		wantNone bool
	}{
		"proxy in env, NO_PROXY from envFrom": {
			env: []corev1.EnvVar{{Name: "HTTPS_PROXY", Value: "http://egress:3128"}}, envFrom: envFrom,
			wantUp: extended, wantLow: extended,
		},
		"all proxy settings from envFrom": {envFrom: envFrom, wantUp: extended, wantLow: extended},
		"NO_PROXY in env wins over envFrom": {
			env:     []corev1.EnvVar{{Name: "NO_PROXY", Value: "10.0.0.0/8"}},
			envFrom: envFrom, wantUp: "10.0.0.0/8," + hosts, wantLow: extended,
		},
		"proxy only in env": {
			env:    []corev1.EnvVar{{Name: "HTTPS_PROXY", Value: "http://egress:3128"}},
			wantUp: hosts, wantLow: hosts,
		},
		"no proxy and no envFrom": {wantNone: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pod := basePod()
			pod.Spec.Containers[0].Env = test.env
			pod.Spec.Containers[0].EnvFrom = test.envFrom

			_, mutated := review(t, newHandler(t), pod)
			require.NotNil(t, mutated)

			upper, foundUpper := env(mutated.Spec.Containers[0], "NO_PROXY")
			lower, foundLower := env(mutated.Spec.Containers[0], "no_proxy")

			if test.wantNone {
				assert.False(t, foundUpper)
				assert.False(t, foundLower)

				return
			}

			assert.Equal(t, test.wantUp, upper)
			assert.Equal(t, test.wantLow, lower)
		})
	}
}
