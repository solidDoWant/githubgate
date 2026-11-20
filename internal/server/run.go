package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	"github.com/solidDoWant/githubgate/internal/progress"
)

const (
	// shutdownTimeout bounds the graceful shutdown after the shutdown delay.
	shutdownTimeout = 20 * time.Second
	// readHeaderTimeout bounds reading a request's headers.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds the TLS handshake and reading a whole request, so a
	// connection that sends nothing cannot hold a slot.
	readTimeout = 10 * time.Second
	// idleTimeout is how long a keep-alive connection may wait for its next request.
	idleTimeout = 60 * time.Second
	// maxConnections is the most connections each listener keeps open; the
	// rest are closed at once.
	maxConnections = 1024
	// proxyProgressTimeout closes a proxy connection when no bytes move in
	// either direction for this long.
	proxyProgressTimeout = 5 * time.Minute
)

// HealthHandler serves /healthz, /readyz and /metrics from gatherer.
func (s *Server) HealthHandler(gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !s.Ready() {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)

			return
		}

		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))

	return mux
}

// RunOptions configure Run.
type RunOptions struct {
	// API receives the token API's connections, served with TLS.
	API net.Listener
	// Health receives /healthz, /readyz and /metrics, served in plain HTTP.
	Health net.Listener
	// CertFile and KeyFile are the token API's certificate and key, read
	// again whenever they change.
	CertFile, KeyFile string
	// ShutdownDelay is how long the token API keeps serving after ctx ends
	// while /readyz already fails, so load balancers stop sending requests.
	ShutdownDelay time.Duration
	// Gatherer is what /metrics serves; prometheus.DefaultGatherer when nil.
	Gatherer prometheus.Gatherer
	// ReadTimeout, IdleTimeout and MaxConnections override the defaults (10s,
	// 60s and 1024 connections per listener) when set.
	ReadTimeout    time.Duration
	IdleTimeout    time.Duration
	MaxConnections int

	// Proxy, when set, receives the proxy's connections, served with TLS by
	// ProxyHandler with ProxyTLS. The proxy has no read or write timeout,
	// so long git transfers aren't cut off; ProxyProgressTimeout bounds stalls.
	Proxy        net.Listener
	ProxyHandler http.Handler
	// ProxyTLS returns the proxy's TLS configuration from the certificate
	// source its files are watched by.
	ProxyTLS func(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config
	// ProxyCertFile and ProxyKeyFile are the proxy's certificate and key,
	// read again whenever they change.
	ProxyCertFile, ProxyKeyFile string
	// ProxyProgressTimeout closes a proxy connection when no bytes move in
	// either direction for this long (5 minutes when zero).
	ProxyProgressTimeout time.Duration
}

// Run serves until ctx ends, then fails /readyz at once, keeps serving for
// the shutdown delay and shuts the servers down gracefully. It refuses to
// start without readable TLS files.
func (s *Server) Run(ctx context.Context, options RunOptions) error {
	watcher, err := certwatcher.New(options.CertFile, options.KeyFile)
	if err != nil {
		return fmt.Errorf("loading the TLS certificate: %w", err)
	}

	if options.Gatherer == nil {
		options.Gatherer = prometheus.DefaultGatherer
	}

	if options.ReadTimeout == 0 {
		options.ReadTimeout = readTimeout
	}

	if options.IdleTimeout == 0 {
		options.IdleTimeout = idleTimeout
	}

	if options.MaxConnections == 0 {
		options.MaxConnections = maxConnections
	}

	if options.ProxyProgressTimeout == 0 {
		options.ProxyProgressTimeout = proxyProgressTimeout
	}

	var proxyWatcher *certwatcher.CertWatcher

	if options.Proxy != nil {
		if proxyWatcher, err = certwatcher.New(options.ProxyCertFile, options.ProxyKeyFile); err != nil {
			return fmt.Errorf("loading the proxy's TLS certificate: %w", err)
		}
	}

	watchCtx, stopWatching := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWatching()

	watchDone := make(chan error, 2)
	watchers := 1

	go func() { watchDone <- watcher.Start(watchCtx) }()

	if proxyWatcher != nil {
		watchers++

		go func() { watchDone <- proxyWatcher.Start(watchCtx) }()
	}

	api := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       options.ReadTimeout,
		IdleTimeout:       options.IdleTimeout,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate},
	}
	health := &http.Server{
		Handler:           s.HealthHandler(options.Gatherer),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       options.ReadTimeout,
		IdleTimeout:       options.IdleTimeout,
	}

	servers := []*http.Server{api, health}
	served := make(chan error, 3)

	go func() { served <- api.ServeTLS(newLimitListener(options.API, options.MaxConnections), "", "") }()
	go func() { served <- health.Serve(newLimitListener(options.Health, options.MaxConnections)) }()

	if proxyWatcher != nil {
		proxy := &http.Server{
			Handler:           options.ProxyHandler,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       options.IdleTimeout,
			TLSConfig:         options.ProxyTLS(proxyWatcher.GetCertificate),
		}
		servers = append(servers, proxy)

		go func() {
			served <- proxy.ServeTLS(newLimitListener(progress.NewListener(options.Proxy, options.ProxyProgressTimeout), options.MaxConnections), "", "")
		}()
	}

	var failure error

	// returned counts the serve goroutines that already finished.
	returned := 0

	select {
	case <-ctx.Done():
		s.draining.Store(true)

		select {
		case <-time.After(options.ShutdownDelay):
		case failure = <-served:
			returned++
		}
	case failure = <-served:
		returned++

		s.draining.Store(true)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err = nil
	for _, server := range servers {
		err = errors.Join(err, server.Shutdown(shutdownCtx))
	}

	// A serve goroutine that had not started when Shutdown ran returns
	// ErrServerClosed as soon as it does, closing its listener. Wait for every
	// one so no listener is open when Run returns.
	for ; returned < len(servers); returned++ {
		if result := <-served; failure == nil {
			failure = result
		}
	}

	stopWatching()

	for range watchers {
		err = errors.Join(err, <-watchDone)
	}

	if failure != nil && !errors.Is(failure, http.ErrServerClosed) {
		return failure
	}

	return err
}
