//go:build integration

package app_test

import (
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
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/githubgate/internal/app"
)

func writeCertificate(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "githubgate"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))

	return dir
}

func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}

func get(t *testing.T, client *http.Client, method, url string) (int, string) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	require.NoError(t, err)

	response, err := client.Do(request)
	if err != nil {
		return 0, ""
	}

	defer func() { _ = response.Body.Close() }()

	buffer := make([]byte, 1<<16)
	n, _ := response.Body.Read(buffer)

	return response.StatusCode, string(buffer[:n])
}

func TestServerServesTheListenersAndTheWebhookAndShutsDown(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	certDir := writeCertificate(t)
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	healthListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	webhookPort := freePort(t)
	delay := 3 * time.Second
	config := app.Config{
		AdminNamespaces: []string{"default"},
		Audience:        "githubgate",
		Webhook:         true,
		WebhookCertDir:  certDir,
		WebhookPort:     webhookPort,
		CertFile:        filepath.Join(certDir, "tls.crt"),
		KeyFile:         filepath.Join(certDir, "tls.key"),
		LeaderElect:     true,
		Namespace:       "default",
		ShutdownDelay:   delay,
		RestConfig:      restConfig,
		API:             apiListener,
		Health:          healthListener,
		Gatherer:        prometheus.NewRegistry(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- app.Run(ctx, config) }()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed test certificate
	}
	healthURL := "http://" + healthListener.Addr().String()

	require.Eventually(t, func() bool {
		status, _ := get(t, client, http.MethodGet, healthURL+"/readyz")

		return status == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond)

	status, _ := get(t, client, http.MethodGet, healthURL+"/healthz")
	assert.Equal(t, http.StatusOK, status)

	status, _ = get(t, client, http.MethodGet, healthURL+"/metrics")
	assert.Equal(t, http.StatusOK, status)

	// Without a bearer token the token API refuses, which shows it is served over TLS.
	status, _ = get(t, client, http.MethodPost, "https://"+apiListener.Addr().String()+"/v1/token")
	assert.Equal(t, http.StatusUnauthorized, status)

	// The webhook answers (a bad review is a denial, not a 404).
	require.Eventually(t, func() bool {
		status, _ := get(t, client, http.MethodPost, "https://127.0.0.1:"+strconv.Itoa(webhookPort)+"/validate")

		return status != 0 && status != http.StatusNotFound
	}, 30*time.Second, 50*time.Millisecond)

	cancel()

	require.Eventually(t, func() bool {
		status, _ := get(t, client, http.MethodGet, healthURL+"/readyz")

		return status == http.StatusServiceUnavailable
	}, 2*time.Second, 10*time.Millisecond)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(25 * time.Second):
		t.Fatal("did not shut down in time")
	}
}
