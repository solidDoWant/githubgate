package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// githubHostsServer starts a TLS server with a self-signed certificate for
// the GitHub hosts and returns it with that certificate as PEM.
func githubHostsServer(t *testing.T, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "stub proxy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"api.github.com", "uploads.github.com", "github.com"},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	require.NoError(t, err)

	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// projectedToken lays out a token the way kubelet's atomic writer does:
// dir/token -> ..data/token, ..data -> a timestamped directory.
type projectedToken struct {
	dir   string
	count int
}

func newProjectedToken(t *testing.T, token string) *projectedToken {
	t.Helper()

	projected := &projectedToken{dir: t.TempDir()}
	projected.write(t, token)
	require.NoError(t, os.Symlink(filepath.Join("..data", "token"), filepath.Join(projected.dir, "token")))

	return projected
}

// write puts token in a new timestamped directory, points ..data_tmp at
// it, and renames ..data_tmp over ..data, as kubelet does on rotation.
func (p *projectedToken) write(t *testing.T, token string) {
	t.Helper()

	p.count++
	target := "..2026_11_20_00_00_0" + string(rune('0'+p.count))

	require.NoError(t, os.Mkdir(filepath.Join(p.dir, target), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(p.dir, target, "token"), []byte(token), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(p.dir, "..data_tmp")))
	require.NoError(t, os.Rename(filepath.Join(p.dir, "..data_tmp"), filepath.Join(p.dir, "..data")))
}

func (p *projectedToken) path() string { return filepath.Join(p.dir, "token") }

func TestInterceptSendsTheRotatedServiceAccountTokenOnTheNextRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)

	proxy, proxyCA := githubHostsServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()

		received = append(received, request.Header.Get("Authorization"))
		mu.Unlock()

		writer.WriteHeader(http.StatusOK)
	}))

	token := newProjectedToken(t, "sa-before-rotation")
	trustDir := filepath.Join(t.TempDir(), "trust")

	addresses := make(chan string, 1)

	var stderr lockedBuffer

	vars := map[string]string{
		"GITHUBGATE_INTERCEPT_PROXY_ADDRESS": proxy.Listener.Addr().String(),
		"GITHUBGATE_INTERCEPT_PROXY_CA":      string(proxyCA),
	}

	env := &environment{
		getenv: func(key string) string { return vars[key] },
		stdin:  strings.NewReader(""),
		stdout: &bytes.Buffer{},
		stderr: &stderr,
		listen: func(ctx context.Context, _ string) (net.Listener, error) {
			var config net.ListenConfig

			listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
			if err == nil {
				addresses <- listener.Addr().String()
			}

			return listener, err
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- run(ctx, env, []string{"intercept",
			"--token-file", token.path(),
			"--trust-dir", trustDir,
			"--state-dir", filepath.Join(t.TempDir(), "state"),
		})
	}()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	address := <-addresses

	require.Eventually(t, func() bool {
		return run(t.Context(), env, []string{"intercept", "--probe", "--listen", address, "--trust-dir", trustDir}) == nil
	}, 10*time.Second, 10*time.Millisecond)

	caPEM, err := os.ReadFile(filepath.Join(trustDir, "ca.crt"))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer

			return dialer.DialContext(ctx, network, address)
		},
	}}

	get := func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/user", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "token ghp_personal")

		response, err := client.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
	}

	get()
	token.write(t, "sa-after-rotation")
	get()

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, []string{"Bearer sa-before-rotation", "Bearer sa-after-rotation"}, received)
	assert.NotContains(t, stderr.String(), "sa-before-rotation")
	assert.NotContains(t, stderr.String(), "ghp_personal")
}

func TestInterceptExitsWhenTheProxyCAHoldsNoCertificate(t *testing.T) {
	tests := []struct {
		name    string
		proxyCA string
	}{
		{name: "empty", proxyCA: ""},
		{name: "not PEM", proxyCA: "not a certificate"},
		{name: "a key only", proxyCA: "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trustDir := filepath.Join(t.TempDir(), "trust")

			_, _, err := runClient(t, map[string]string{
				"GITHUBGATE_INTERCEPT_PROXY_ADDRESS": "127.0.0.1:1",
				"GITHUBGATE_INTERCEPT_PROXY_CA":      test.proxyCA,
			}, "", "intercept", "--listen", "127.0.0.1:0", "--trust-dir", trustDir, "--state-dir", t.TempDir())
			require.Error(t, err)

			_, err = os.Stat(trustDir)
			assert.ErrorIs(t, err, os.ErrNotExist, "nothing is written")
		})
	}
}

func TestInterceptReadsTheProxyCAFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-ca.crt")
	require.NoError(t, os.WriteFile(path, []byte("no certificate"), 0o600))

	_, _, err := runClient(t, map[string]string{"GITHUBGATE_INTERCEPT_PROXY_ADDRESS": "127.0.0.1:1"},
		"", "intercept", "--listen", "127.0.0.1:0", "--proxy-ca", "@"+path, "--state-dir", t.TempDir(), "--trust-dir", t.TempDir())
	require.ErrorContains(t, err, "no certificate")

	_, _, err = runClient(t, map[string]string{"GITHUBGATE_INTERCEPT_PROXY_ADDRESS": "127.0.0.1:1"},
		"", "intercept", "--listen", "127.0.0.1:0", "--proxy-ca", "@"+filepath.Join(t.TempDir(), "missing"), "--state-dir", t.TempDir(), "--trust-dir", t.TempDir())
	require.Error(t, err)
}

// lockedBuffer is a bytes.Buffer safe for the interceptor's logger and the
// test to use at once.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}
