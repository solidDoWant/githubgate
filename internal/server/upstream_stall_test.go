package server_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/internal/server"
)

type staticTokens struct{}

func (staticTokens) ProxyToken(context.Context, auth.Identity, proxy.Decision) (string, string, error) {
	return "token", "minted", nil
}

func TestRunEndsRequestAndClosesUpstreamWhenUpstreamResponseStalls(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)
	proxyCertFile, proxyKeyFile := writeCertificate(t, t.TempDir(), 7)

	// The upstream sends the headers and part of the body, then stops.
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "1000")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("partial"))
		writer.(http.Flusher).Flush()

		<-request.Context().Done()
		close(upstreamClosed)
	}))
	t.Cleanup(upstream.Close)

	base, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	installation := &model.Installation{NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "app-acme"}, Owner: "acme", Ready: true}
	rule := &model.Rule{
		Policy: types.NamespacedName{Namespace: "ci-a", Name: "policy"}, Name: "a", Installation: installation,
		Repositories: []string{"foo"}, Permissions: map[string]permissions.Level{"contents": permissions.LevelRead}, Proxy: &model.ProxyRule{},
	}

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:     func(string, string) []*model.Rule { return []*model.Rule{rule} },
		Tokens:    staticTokens{},
		Upstreams: proxy.Upstreams{API: base, Uploads: base, Git: base},
		Transport: upstream.Client().Transport,
	})
	require.NoError(t, err)

	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	healthListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- f.server.Run(ctx, server.RunOptions{
			API: apiListener, Health: healthListener, CertFile: certFile, KeyFile: keyFile, Gatherer: prometheus.NewRegistry(),
			ProxyProgressTimeout: time.Second,
			Proxy:                proxyListener, ProxyCertFile: proxyCertFile, ProxyKeyFile: proxyKeyFile, ProxyTLS: proxy.TLSConfig,
			ProxyHandler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				forwarder.Forward(writer, request, auth.Identity{Namespace: "ci-a", ServiceAccount: "runner"},
					proxy.Request{Host: proxy.HostAPI, Kind: proxy.KindREST, Operation: "repos/get", Owner: "acme", Repo: "foo", Path: "/repos/acme/foo"})
			}),
		})
	}()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	address := proxyListener.Addr().String()

	require.Eventually(t, func() bool {
		serial, err := servedProxySerial(address, proxy.HostAPI)

		return err == nil && serial == 7
	}, 10*time.Second, 20*time.Millisecond)

	connection, err := tls.Dial("tcp", address, &tls.Config{ServerName: proxy.HostAPI, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // see servedSerial
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })

	_, err = connection.Write([]byte("GET /repos/acme/foo HTTP/1.1\r\nHost: api.github.com\r\n\r\n"))
	require.NoError(t, err)

	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	// The body never completes: the proxy ends the response.
	require.Eventually(t, func() bool {
		_ = connection.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := io.ReadAll(response.Body)

		var netErr net.Error

		return err != nil && (!errors.As(err, &netErr) || !netErr.Timeout())
	}, 15*time.Second, 50*time.Millisecond, "the response to the client never ended")

	select {
	case <-upstreamClosed:
	case <-time.After(15 * time.Second):
		t.Fatal("the upstream connection stayed open")
	}
}
