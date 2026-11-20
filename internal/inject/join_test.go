package inject_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/solidDoWant/githubgate/internal/inject"
)

type fakeCluster struct {
	annotations  map[string]string
	namespaceErr error
	configMaps   map[string]bool
}

func (c fakeCluster) NamespaceAnnotations(context.Context, string) (map[string]string, error) {
	return c.annotations, c.namespaceErr
}

func (c fakeCluster) ConfigMapHasKey(_ context.Context, _, name, _ string) (bool, error) {
	return c.configMaps[name], nil
}

func newClusterHandler(t *testing.T, cluster inject.Cluster) *inject.Handler {
	t.Helper()

	return inject.New(inject.Config{
		Image: "ghcr.io/example/client@sha256:abc", ProxyAddress: "githubgate-proxy.gg.svc:443",
		Audience: "githubgate", ProxyCAFile: writeCA(t), Cluster: cluster,
	})
}

func joinPod(source corev1.VolumeSource, mount corev1.VolumeMount, variable, value string) *corev1.Pod {
	pod := basePod()
	pod.Spec.Volumes = []corev1.Volume{{Name: mount.Name, VolumeSource: source}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{mount}
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: variable, Value: value}}

	return pod
}

func TestJoinsContainerCAVariable(t *testing.T) {
	t.Parallel()

	configMap := corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}

	tests := map[string]struct {
		source   corev1.VolumeSource
		mount    corev1.VolumeMount
		variable string
		value    string
		args     []string
		mounted  corev1.VolumeMount
	}{
		"configmap directory": {
			configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca/ca.pem",
			[]string{"intercept", "--join", "main/SSL_CERT_FILE=/var/run/githubgate/join/main/0/ca.pem"},
			corev1.VolumeMount{Name: "ca", MountPath: "/var/run/githubgate/join/main/0", ReadOnly: true},
		},
		"secret with subPath file": {
			corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ssl/own.pem", SubPath: "own.pem"}, "NODE_EXTRA_CA_CERTS", "/etc/ssl/own.pem",
			[]string{"intercept", "--join", "main/NODE_EXTRA_CA_CERTS=/var/run/githubgate/join/main/0"},
			corev1.VolumeMount{Name: "ca", MountPath: "/var/run/githubgate/join/main/0", SubPath: "own.pem", ReadOnly: true},
		},
		"projected": {
			corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}, corev1.VolumeMount{Name: "ca", MountPath: "/ca"}, "CURL_CA_BUNDLE", "/ca/sub/../b.pem",
			[]string{"intercept", "--join", "main/CURL_CA_BUNDLE=/var/run/githubgate/join/main/0/b.pem"},
			corev1.VolumeMount{Name: "ca", MountPath: "/var/run/githubgate/join/main/0", ReadOnly: true},
		},
		"emptyDir": {
			corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}, corev1.VolumeMount{Name: "ca", MountPath: "/shared"}, "GIT_SSL_CAINFO", "/shared/ca.crt",
			[]string{"intercept", "--join", "main/GIT_SSL_CAINFO=/var/run/githubgate/join/main/0/ca.crt"},
			corev1.VolumeMount{Name: "ca", MountPath: "/var/run/githubgate/join/main/0", ReadOnly: true},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			response, mutated := review(t, newHandler(t), joinPod(test.source, test.mount, test.variable, test.value))
			require.True(t, response.Allowed, response.Result)

			main := mutated.Spec.Containers[0]
			value, _ := env(main, test.variable)
			assert.Equal(t, trust+"/join/main/"+test.variable, value)

			count := 0

			for _, variable := range main.Env {
				if variable.Name == test.variable {
					count++
				}
			}

			assert.Equal(t, 1, count)

			sidecar := mutated.Spec.InitContainers[0]
			assert.Equal(t, test.args, sidecar.Args)
			assert.Contains(t, sidecar.VolumeMounts, test.mounted)
			assert.Contains(t, main.VolumeMounts, test.mount, "the container's own mount stays")
		})
	}

	t.Run("other variables still point at the pod bundle", func(t *testing.T) {
		t.Parallel()

		_, mutated := review(t, newHandler(t), joinPod(configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca/x"))
		value, _ := env(mutated.Spec.Containers[0], "CURL_CA_BUNDLE")
		assert.Equal(t, trust+"/bundle.crt", value)
	})
}

func TestRejectsCAVariableItCannotJoin(t *testing.T) {
	t.Parallel()

	configMap := corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}
	message := "githubgate: pod can't be intercepted: container \"main\" sets SSL_CERT_FILE to a file githubgate can't read; put the CA in a ConfigMap and name it in the namespace annotation githubgate.soliddowant.dev/extra-ca-configmap"

	tests := map[string]*corev1.Pod{
		"file in the image":                      joinPod(configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ssl/certs/ca.pem"),
		"outside every mount by a shared prefix": joinPod(configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca2/ca.pem"),
		"path climbs out of the mount":           joinPod(configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca/../ssl/ca.pem"),
		"hostPath volume":                        joinPod(corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/x"}}, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca/ca.pem"),
		"subPathExpr":                            joinPod(configMap, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca", SubPathExpr: "$(X)"}, "SSL_CERT_FILE", "/etc/ca/ca.pem"),
		"valueFrom": func() *corev1.Pod {
			pod := basePod()
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "SSL_CERT_FILE", ValueFrom: &corev1.EnvVarSource{}}}

			return pod
		}(),
	}

	for name, pod := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			response, _ := review(t, newHandler(t), pod)
			require.False(t, response.Allowed)
			assert.Equal(t, message, response.Result.Message)
		})
	}
}

func TestJoinIsIdempotentOnReinvocation(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)
	pod := joinPod(corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}, corev1.VolumeMount{Name: "ca", MountPath: "/etc/ca"}, "SSL_CERT_FILE", "/etc/ca/ca.pem")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:         "second",
		VolumeMounts: []corev1.VolumeMount{{Name: "ca", MountPath: "/other"}},
		Env:          []corev1.EnvVar{{Name: "REQUESTS_CA_BUNDLE", Value: "/other/b.pem"}},
	})

	_, once := review(t, handler, pod)
	require.NotNil(t, once)

	// Another webhook adds a container with its own CA variable.
	once.Spec.Containers = append(once.Spec.Containers, corev1.Container{
		Name:         "late",
		VolumeMounts: []corev1.VolumeMount{{Name: "ca", MountPath: "/late"}},
		Env:          []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: "/late/c.pem"}},
	})

	_, twice := review(t, handler, once)
	require.NotNil(t, twice)

	_, third := review(t, handler, twice)
	require.NotNil(t, third)

	assert.Equal(t, twice.Spec, third.Spec)
	assert.Equal(t, []string{
		"intercept",
		"--join", "main/SSL_CERT_FILE=/var/run/githubgate/join/main/0/ca.pem",
		"--join", "second/REQUESTS_CA_BUNDLE=/var/run/githubgate/join/second/0/b.pem",
		"--join", "late/SSL_CERT_FILE=/var/run/githubgate/join/late/0/c.pem",
	}, twice.Spec.InitContainers[0].Args)

	value, _ := env(twice.Spec.Containers[0], "SSL_CERT_FILE")
	assert.Equal(t, trust+"/join/main/SSL_CERT_FILE", value)
}

func TestNamespaceExtraCA(t *testing.T) {
	t.Parallel()

	annotated := map[string]string{inject.ExtraCAConfigMapAnnotation: "corp-ca"}

	t.Run("mounts the ConfigMap into the interceptor", func(t *testing.T) {
		t.Parallel()

		handler := newClusterHandler(t, fakeCluster{annotations: annotated, configMaps: map[string]bool{"corp-ca": true}})

		response, mutated := review(t, handler, basePod())
		require.True(t, response.Allowed)

		sidecar := mutated.Spec.InitContainers[0]
		assert.Equal(t, []string{"intercept", "--extra-ca", "@/var/run/githubgate/extra-ca/ca.crt"}, sidecar.Args)
		assert.Contains(t, sidecar.VolumeMounts, corev1.VolumeMount{Name: "githubgate-extra-ca", MountPath: "/var/run/githubgate/extra-ca", ReadOnly: true})

		for _, volume := range mutated.Spec.Volumes {
			if volume.Name == "githubgate-extra-ca" {
				assert.Equal(t, "corp-ca", volume.ConfigMap.Name)
				assert.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}, volume.ConfigMap.Items)

				// Reinvocation keeps one copy.
				_, again := review(t, handler, mutated)
				assert.Equal(t, mutated.Spec, again.Spec)

				return
			}
		}

		assert.Fail(t, "extra CA volume missing")
	})

	t.Run("a missing ConfigMap rejects the pod and names it", func(t *testing.T) {
		t.Parallel()

		response, _ := review(t, newClusterHandler(t, fakeCluster{annotations: annotated}), basePod())
		require.False(t, response.Allowed)
		assert.Contains(t, response.Result.Message, `ConfigMap "corp-ca"`)
	})

	t.Run("no annotation adds nothing", func(t *testing.T) {
		t.Parallel()

		_, mutated := review(t, newClusterHandler(t, fakeCluster{}), basePod())
		assert.Equal(t, []string{"intercept"}, mutated.Spec.InitContainers[0].Args)
	})

	t.Run("an unreadable namespace denies", func(t *testing.T) {
		t.Parallel()

		response, _ := review(t, newClusterHandler(t, fakeCluster{namespaceErr: errors.New("boom")}), basePod())
		assert.False(t, response.Allowed)
	})
}
