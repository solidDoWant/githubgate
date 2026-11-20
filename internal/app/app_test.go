package app_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/app"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestParseFlags(t *testing.T) {
	base := []string{"--tls-cert-file=/c", "--tls-key-file=/k"}

	tests := map[string]struct {
		args   []string
		env    map[string]string
		check  func(*testing.T, app.Config)
		assert require.ErrorAssertionFunc
	}{
		"defaults to the own namespace as admin namespace": {
			args: base, env: map[string]string{"POD_NAMESPACE": "githubgate"},
			check: func(t *testing.T, config app.Config) {
				assert.Equal(t, []string{"githubgate"}, config.AdminNamespaces)
				assert.Equal(t, "githubgate", config.Audience)
				assert.Equal(t, 5*time.Second, config.ShutdownDelay)
				assert.Equal(t, ":8443", config.APIAddress)
				assert.Equal(t, ":8080", config.HealthAddress)
				assert.False(t, config.Webhook)
			},
		},
		"explicit flags": {
			args: append([]string{"--admin-namespaces=a, b", "--audience=x", "--shutdown-delay=1s", "--webhook", "--webhook-cert-dir=/w", "--leader-elect"}, base...),
			env:  map[string]string{"POD_NAMESPACE": "n"},
			check: func(t *testing.T, config app.Config) {
				assert.Equal(t, []string{"a", "b"}, config.AdminNamespaces)
				assert.Equal(t, "x", config.Audience)
				assert.Equal(t, time.Second, config.ShutdownDelay)
				assert.True(t, config.Webhook)
				assert.True(t, config.LeaderElect)
			},
		},
		"proxy flags": {
			args: append([]string{"--admin-namespaces=a", "--proxy", "--proxy-tls-cert-file=/pc", "--proxy-tls-key-file=/pk"}, base...),
			check: func(t *testing.T, config app.Config) {
				assert.True(t, config.Proxy)
				assert.Equal(t, ":9443", config.ProxyAddress)
				assert.Equal(t, "/pc", config.ProxyCertFile)
				assert.Equal(t, "/pk", config.ProxyKeyFile)
			},
		},
		"proxy without its TLS files": {args: append([]string{"--admin-namespaces=a", "--proxy"}, base...), assert: require.Error},
		"proxy and webhook on one port": {
			args:   append([]string{"--admin-namespaces=a", "--proxy", "--proxy-tls-cert-file=/pc", "--proxy-tls-key-file=/pk", "--webhook", "--webhook-cert-dir=/w"}, base...),
			assert: require.Error,
		},
		"proxy and webhook on different ports": {
			args: append([]string{
				"--admin-namespaces=a", "--proxy", "--proxy-tls-cert-file=/pc", "--proxy-tls-key-file=/pk",
				"--webhook", "--webhook-cert-dir=/w", "--webhook-port=9444",
			}, base...),
		},
		"inject with defaults": {
			args: append([]string{"--inject", "--webhook-cert-dir=/w", "--inject-image=ghcr.io/x/c@sha256:abc", "--inject-proxy-ca-file=/ca"}, base...),
			env:  map[string]string{"POD_NAMESPACE": "gg"},
			check: func(t *testing.T, config app.Config) {
				assert.True(t, config.Inject)
				assert.False(t, config.Webhook)
				assert.Equal(t, "githubgate-proxy.gg.svc:443", config.InjectProxyAddress)
			},
		},
		"inject without image": {
			args:   append([]string{"--inject", "--webhook-cert-dir=/w", "--inject-proxy-ca-file=/ca"}, base...),
			env:    map[string]string{"POD_NAMESPACE": "gg"},
			assert: require.Error,
		},
		"inject image not pinned": {
			args:   append([]string{"--inject", "--webhook-cert-dir=/w", "--inject-image=ghcr.io/x/c:latest", "--inject-proxy-ca-file=/ca"}, base...),
			env:    map[string]string{"POD_NAMESPACE": "gg"},
			assert: require.Error,
		},
		"inject without proxy CA file": {
			args:   append([]string{"--inject", "--webhook-cert-dir=/w", "--inject-image=ghcr.io/x/c@sha256:abc"}, base...),
			env:    map[string]string{"POD_NAMESPACE": "gg"},
			assert: require.Error,
		},
		"inject without webhook dir": {
			args:   append([]string{"--inject", "--inject-image=ghcr.io/x/c@sha256:abc", "--inject-proxy-ca-file=/ca"}, base...),
			env:    map[string]string{"POD_NAMESPACE": "gg"},
			assert: require.Error,
		},
		"no admin namespace":  {args: base, assert: require.Error},
		"missing TLS flags":   {args: []string{"--admin-namespaces=a"}, assert: require.Error},
		"webhook without dir": {args: append([]string{"--admin-namespaces=a", "--webhook"}, base...), assert: require.Error},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.assert == nil {
				test.assert = require.NoError
			}

			config, err := app.ParseFlags(test.args, env(test.env))
			test.assert(t, err)

			if test.check != nil {
				test.check(t, config)
			}
		})
	}
}

func TestRunNamesTheUnreadableTLSFile(t *testing.T) {
	err := app.Run(t.Context(), app.Config{
		AdminNamespaces: []string{"a"},
		CertFile:        "/does/not/exist.crt",
		KeyFile:         "/does/not/exist.key",
	})
	require.ErrorContains(t, err, "/does/not/exist.crt")
}
