package intercept

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interceptor is a running interceptor in front of a stub proxy.
type interceptor struct {
	address  string
	trustDir string
	stateDir string
	stub     *stubProxy
	done     chan error
}

func startInterceptor(t *testing.T, extraCA []byte) *interceptor {
	t.Helper()

	stub := newStubProxy(t, nil)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	running := &interceptor{
		address:  listener.Addr().String(),
		trustDir: filepath.Join(t.TempDir(), "trust"),
		stateDir: filepath.Join(t.TempDir(), "state"),
		stub:     stub,
		done:     make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		running.done <- Run(ctx, Options{
			Listener:     listener,
			ProxyAddress: stub.address(),
			ProxyCA:      stub.caPEM,
			TokenFile:    writeToken(t, "sa-token"),
			TrustDir:     running.trustDir,
			StateDir:     running.stateDir,
			ExtraCA:      extraCA,
		})
	}()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-running.done)
	})

	require.Eventually(t, func() bool {
		return Probe(t.Context(), running.address, running.trustDir) == nil
	}, 10*time.Second, 10*time.Millisecond)

	return running
}

// client trusts only the trust directory's ca.crt and dials the
// interceptor for every host.
func (i *interceptor) client(t *testing.T, serverName string, http2 bool) *http.Client {
	t.Helper()

	caPEM, err := os.ReadFile(filepath.Join(i.trustDir, "ca.crt"))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer

			return dialer.DialContext(ctx, network, i.address)
		},
		ForceAttemptHTTP2: http2,
	}
	if !http2 {
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}

	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport}
}

func TestRunRefusesAProxyCAWithoutCertificates(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	err = Run(t.Context(), Options{
		Listener:     listener,
		ProxyAddress: "127.0.0.1:1",
		ProxyCA:      []byte("no certificate here"),
		TokenFile:    writeToken(t, "sa-token"),
		TrustDir:     t.TempDir(),
		StateDir:     t.TempDir(),
	})
	require.Error(t, err)
}

func TestRunForwardsOverHTTP1AndHTTP2(t *testing.T) {
	running := startInterceptor(t, nil)

	for _, http2 := range []bool{false, true} {
		response, err := running.client(t, "api.github.com", http2).Get("https://api.github.com/repos/acme/foo")
		require.NoError(t, err)

		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())

		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, "from the proxy", string(body))

		if http2 {
			assert.Equal(t, 2, response.ProtoMajor)
		} else {
			assert.Equal(t, 1, response.ProtoMajor)
		}
	}
}

func TestRunUsesTheHostHeaderWhenItDiffersFromTheServerName(t *testing.T) {
	running := startInterceptor(t, nil)

	response, err := running.client(t, "api.github.com", true).Get("https://github.com/acme/foo.git/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)

	seen := running.stub.requests()
	require.Len(t, seen, 1)
	assert.Equal(t, "github.com", seen[0].Host)
	assert.Equal(t, "github.com", seen[0].ServerName)
}

func TestRunFailsTheHandshakeForOtherServerNames(t *testing.T) {
	running := startInterceptor(t, nil)

	caPEM, err := os.ReadFile(filepath.Join(running.trustDir, "ca.crt"))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	for _, serverName := range []string{"example.com", "gist.github.com", ""} {
		dialer := &tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: serverName, InsecureSkipVerify: serverName == "", MinVersion: tls.VersionTLS12}} //nolint:gosec // an empty server name can't be verified; the handshake must fail anyway

		conn, err := dialer.DialContext(t.Context(), "tcp", running.address)
		if err == nil {
			_ = conn.Close()
		}

		assert.Error(t, err, "server name %q", serverName)
	}

	assert.Empty(t, running.stub.requests())
}

func TestRunWritesTheTrustFiles(t *testing.T) {
	extra, err := newCA(time.Now())
	require.NoError(t, err)

	running := startInterceptor(t, extra.PEM)

	caPEM, err := os.ReadFile(filepath.Join(running.trustDir, "ca.crt"))
	require.NoError(t, err)

	statePEM, err := os.ReadFile(filepath.Join(running.stateDir, "ca.crt"))
	require.NoError(t, err)
	assert.Equal(t, string(statePEM), string(caPEM), "ca.crt is the pod CA only")

	bundled, err := os.ReadFile(filepath.Join(running.trustDir, "bundle.crt"))
	require.NoError(t, err)

	base := DefaultBaseBundle()
	require.NotEmpty(t, base)
	assert.Equal(t, string(base)+string(extra.PEM)+string(caPEM), string(bundled))

	entries, err := os.ReadDir(running.trustDir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		names = append(names, entry.Name())

		info, err := entry.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o444), info.Mode().Perm(), entry.Name())

		data, err := os.ReadFile(filepath.Join(running.trustDir, entry.Name()))
		require.NoError(t, err)

		for len(data) > 0 {
			var block *pem.Block

			block, data = pem.Decode(data)
			if block == nil {
				break
			}

			assert.Equal(t, "CERTIFICATE", block.Type, entry.Name())
		}
	}

	assert.ElementsMatch(t, []string{"ca.crt", "bundle.crt", "ready"}, names)
}

func TestRunReusesTheCAAcrossRestarts(t *testing.T) {
	stateDir := t.TempDir()

	first, err := LoadOrCreateCA(stateDir, time.Now())
	require.NoError(t, err)

	stub := newStubProxy(t, nil)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	trustDir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, Options{
			Listener: listener, ProxyAddress: stub.address(), ProxyCA: stub.caPEM,
			TokenFile: writeToken(t, "sa-token"), TrustDir: trustDir, StateDir: stateDir,
		})
	}()

	require.Eventually(t, func() bool { return Probe(t.Context(), listener.Addr().String(), trustDir) == nil },
		10*time.Second, 10*time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	written, err := os.ReadFile(filepath.Join(trustDir, "ca.crt"))
	require.NoError(t, err)
	assert.Equal(t, string(first.PEM), string(written))
}

func TestRunRefusesAnExtraCAThatIsNotOnlyCertificates(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	stub := newStubProxy(t, nil)
	trustDir := t.TempDir()

	err = Run(t.Context(), Options{
		Listener: listener, ProxyAddress: stub.address(), ProxyCA: stub.caPEM,
		ExtraCA:   append(append([]byte(nil), stub.caPEM...), []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")...),
		TokenFile: writeToken(t, "sa-token"), TrustDir: trustDir, StateDir: t.TempDir(),
	})
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(trustDir, "bundle.crt"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestProbeFailsWithoutTheReadyFileOrAVerifyingInterceptor(t *testing.T) {
	running := startInterceptor(t, nil)

	require.NoError(t, Probe(t.Context(), running.address, running.trustDir))

	t.Run("ready missing", func(t *testing.T) {
		trustDir := t.TempDir()

		caPEM, err := os.ReadFile(filepath.Join(running.trustDir, "ca.crt"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(trustDir, "ca.crt"), caPEM, 0o444))

		assert.Error(t, Probe(t.Context(), running.address, trustDir))
	})

	t.Run("another CA", func(t *testing.T) {
		trustDir := t.TempDir()

		other, err := newCA(time.Now())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(trustDir, "ca.crt"), other.PEM, 0o444))
		require.NoError(t, os.WriteFile(filepath.Join(trustDir, "ready"), nil, 0o444))

		assert.Error(t, Probe(t.Context(), running.address, trustDir))
	})

	t.Run("nothing listening", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		assert.Error(t, Probe(t.Context(), address, running.trustDir))
	})
}
