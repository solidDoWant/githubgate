package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/server"
)

// writeCertificate writes a self-signed certificate for 127.0.0.1 with the
// given serial number, replacing the files atomically.
func writeCertificate(t *testing.T, dir string, serial int64) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "githubgate"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for file, block := range map[string]*pem.Block{
		certFile: {Type: "CERTIFICATE", Bytes: der},
		keyFile:  {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		temporary := file + ".new"
		require.NoError(t, os.WriteFile(temporary, pem.EncodeToMemory(block), 0o600))
		require.NoError(t, os.Rename(temporary, file))
	}

	return certFile, keyFile
}

type running struct {
	api, health string
	done        chan error
	cancel      context.CancelFunc
}

func startRun(t *testing.T, f *fixture, certFile, keyFile string, delay time.Duration) running {
	t.Helper()

	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	healthListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	r := running{
		api:    apiListener.Addr().String(),
		health: healthListener.Addr().String(),
		done:   make(chan error, 1),
		cancel: cancel,
	}

	go func() {
		r.done <- f.server.Run(ctx, server.RunOptions{
			API: apiListener, Health: healthListener, CertFile: certFile, KeyFile: keyFile,
			ShutdownDelay: delay, Gatherer: prometheus.NewRegistry(),
		})
	}()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-r.done)
	})

	return r
}

func servedSerial(t *testing.T, address string) int64 {
	t.Helper()

	connection, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test reads the certificate served, it doesn't trust it
	require.NoError(t, err)

	defer func() { assert.NoError(t, connection.Close()) }()

	return connection.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func status(t *testing.T, client *http.Client, method, url string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(`{}`))
	require.NoError(t, err)

	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	return response.StatusCode
}

func TestRunRefusesToStartWithoutTLSFiles(t *testing.T) {
	f := newFixture(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { assert.NoError(t, listener.Close()) }()

	missing := filepath.Join(t.TempDir(), "missing")
	err = f.server.Run(t.Context(), server.RunOptions{API: listener, Health: listener, CertFile: missing, KeyFile: missing})
	require.Error(t, err)
}

func TestRunServesANewCertificateAfterTheFilesChange(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	certFile, keyFile := writeCertificate(t, dir, 1)

	r := startRun(t, f, certFile, keyFile, 0)

	require.Eventually(t, func() bool {
		connection, err := tls.Dial("tcp", r.api, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // see servedSerial
		if err == nil {
			assert.NoError(t, connection.Close())
		}

		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, int64(1), servedSerial(t, r.api))

	writeCertificate(t, dir, 2)

	require.Eventually(t, func() bool { return servedSerial(t, r.api) == 2 }, 30*time.Second, 50*time.Millisecond)
}

func TestRunFailsReadinessAtOnceAndKeepsServingForTheShutdownDelay(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)

	const delay = 3 * time.Second

	r := startRun(t, f, certFile, keyFile, delay)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see servedSerial
		DisableKeepAlives: true,
	}}

	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + r.health + "/readyz") //nolint:noctx // polled until the server is up
		if err != nil {
			return false
		}

		assert.NoError(t, response.Body.Close())

		return response.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond)

	cancelled := time.Now()

	r.cancel()

	require.Eventually(t, func() bool {
		return status(t, client, http.MethodGet, "http://"+r.health+"/readyz") == http.StatusServiceUnavailable
	}, 10*time.Second, 10*time.Millisecond)

	// The token API still answers during the shutdown delay.
	assert.Equal(t, http.StatusUnauthorized, status(t, client, http.MethodPost, "https://"+r.api+"/v1/token"))

	select {
	case err := <-r.done:
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(cancelled), delay, "Run returned before the shutdown delay ended")

		r.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the shutdown delay")
	}
}

func TestRunShutsDownGracefullyAfterTheShutdownDelay(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)

	r := startRun(t, f, certFile, keyFile, 0)
	r.cancel()

	select {
	case err := <-r.done:
		require.NoError(t, err)

		r.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the shutdown delay")
	}

	_, err := net.Dial("tcp", r.api)
	require.Error(t, err)
}

func TestRunClosesIdleConnectionsAndRefusesExcessOnes(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)

	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	healthListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- f.server.Run(ctx, server.RunOptions{
			API: apiListener, Health: healthListener, CertFile: certFile, KeyFile: keyFile,
			Gatherer: prometheus.NewRegistry(), ReadTimeout: 8 * time.Second, IdleTimeout: 500 * time.Millisecond, MaxConnections: 3,
		})
	}()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	// closed reports whether the server closes a plain connection within the deadline.
	closed := func(connection net.Conn, within time.Duration) bool {
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(within)))

		_, err := connection.Read(make([]byte, 1))

		return !errors.Is(err, os.ErrDeadlineExceeded)
	}

	// Idle health connection after a request is closed.
	idle, err := net.Dial("tcp", healthListener.Addr().String())
	require.NoError(t, err)

	defer func() { _ = idle.Close() }()

	_, err = idle.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"))
	require.NoError(t, err)

	buffer := make([]byte, 4096)
	_, err = idle.Read(buffer)
	require.NoError(t, err)
	assert.True(t, closed(idle, 4*time.Second), "idle keep-alive connection stays open")

	// Connections over the limit are closed at once while the held ones stay.
	held := make([]net.Conn, 3)
	for index := range held {
		held[index], err = net.Dial("tcp", apiListener.Addr().String())
		require.NoError(t, err)

		defer func() { _ = held[index].Close() }()
	}

	require.Eventually(t, func() bool {
		extra, err := net.Dial("tcp", apiListener.Addr().String())
		if err != nil {
			return false
		}

		defer func() { _ = extra.Close() }()

		return closed(extra, 2*time.Second)
	}, 5*time.Second, 10*time.Millisecond)

	// Held connections that never send anything are closed too, which frees
	// the slots for a real caller.
	require.Eventually(t, func() bool {
		connection, err := tls.Dial("tcp", apiListener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test
		if err != nil {
			return false
		}

		return connection.Close() == nil
	}, 20*time.Second, 50*time.Millisecond)
}
