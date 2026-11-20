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
)

const (
	// shutdownTimeout bounds the graceful shutdown after the shutdown delay.
	shutdownTimeout = 20 * time.Second
	// readHeaderTimeout bounds reading a request's headers.
	readHeaderTimeout = 10 * time.Second
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

	watchCtx, stopWatching := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWatching()

	watchDone := make(chan error, 1)

	go func() { watchDone <- watcher.Start(watchCtx) }()

	api := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate},
	}
	health := &http.Server{Handler: s.HealthHandler(options.Gatherer), ReadHeaderTimeout: readHeaderTimeout}

	served := make(chan error, 2)

	go func() { served <- api.ServeTLS(options.API, "", "") }()
	go func() { served <- health.Serve(options.Health) }()

	var failure error

	select {
	case <-ctx.Done():
		s.draining.Store(true)

		select {
		case <-time.After(options.ShutdownDelay):
		case failure = <-served:
		}
	case failure = <-served:
		s.draining.Store(true)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err = errors.Join(api.Shutdown(shutdownCtx), health.Shutdown(shutdownCtx))

	stopWatching()

	err = errors.Join(err, <-watchDone)

	if failure != nil && !errors.Is(failure, http.ErrServerClosed) {
		return failure
	}

	return err
}
