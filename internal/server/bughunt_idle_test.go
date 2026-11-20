package server_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/internal/server"
)

// A client that sends a request's headers and part of its body, then sends
// nothing more, should be cut off after the design's 5-minute idle-progress
// timeout. This checks the connection is still open after 6 minutes.
func TestBughuntProxyStalledBodyIsNeverCutOff(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)
	proxyCertFile, proxyKeyFile := writeCertificate(t, t.TempDir(), 7)

	apiListener, _ := net.Listen("tcp", "127.0.0.1:0")
	healthListener, _ := net.Listen("tcp", "127.0.0.1:0")
	proxyListener, _ := net.Listen("tcp", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = f.server.Run(ctx, server.RunOptions{
			API: apiListener, Health: healthListener, CertFile: certFile, KeyFile: keyFile, Gatherer: prometheus.NewRegistry(),
			Proxy: proxyListener, ProxyCertFile: proxyCertFile, ProxyKeyFile: proxyKeyFile, ProxyTLS: proxy.TLSConfig,
			ProxyHandler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.ReadAll(request.Body)
				writer.WriteHeader(http.StatusTeapot)
			}),
		})
	}()

	var connection *tls.Conn
	require.Eventually(t, func() bool {
		var err error
		connection, err = tls.Dial("tcp", proxyListener.Addr().String(), &tls.Config{ServerName: proxy.HostAPI, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // test
		return err == nil
	}, 10*time.Second, 50*time.Millisecond)

	_, err := connection.Write([]byte("POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 1000\r\n\r\n{\"query\""))
	require.NoError(t, err)

	started := time.Now()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(6*time.Minute)))
	_, err = connection.Read(make([]byte, 1))
	t.Logf("read after %s: %v", time.Since(started), err)

	var netError net.Error
	require.ErrorAs(t, err, &netError)
	require.True(t, netError.Timeout(), "the server closed the stalled connection (good): %v", err)
	t.Errorf("the proxy kept a connection with no progress open for %s", time.Since(started))
}
