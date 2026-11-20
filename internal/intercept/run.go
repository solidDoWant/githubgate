package intercept

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solidDoWant/githubgate/internal/progress"
)

const (
	// readHeaderTimeout, idleTimeout and progressTimeout are the proxy's own
	// server timeouts.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	progressTimeout   = 5 * time.Minute
	shutdownTimeout   = 10 * time.Second
	probeTimeout      = 5 * time.Second
)

// Options configure Run.
type Options struct {
	// Listener receives the workload's connections.
	Listener net.Listener
	// ProxyAddress is the proxy's host:port; every request is sent there.
	ProxyAddress string
	// ProxyCA holds the only roots used to verify the proxy.
	ProxyCA []byte
	// TokenFile holds the pod's ServiceAccount token.
	TokenFile string
	// TrustDir receives ca.crt, bundle.crt and ready.
	TrustDir string
	// StateDir holds the CA's key and certificate.
	StateDir string
	// BaseBundle is the PEM roots bundle.crt starts with;
	// DefaultBaseBundle when nil.
	BaseBundle []byte
	// ExtraCA is PEM appended to bundle.crt; may be empty.
	ExtraCA []byte
	// Joins are the files written to TrustDir/join: a container's own CA
	// file followed by the pod CA, rewritten when the source changes.
	Joins []Join
	// JoinInterval is how often the sources are read; DefaultJoinInterval
	// when zero.
	JoinInterval time.Duration
	// Logger receives start, stop and errors.
	Logger *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// ProxyCAPool returns a pool holding only the certificates in data, and
// fails when there are none.
func ProxyCAPool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("the proxy CA holds no certificate")
	}

	return pool, nil
}

// Run sets up the CA, the leaf and the trust files, then serves until ctx
// ends.
func Run(ctx context.Context, options Options) error {
	if options.Now == nil {
		options.Now = time.Now
	}

	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}

	pool, err := ProxyCAPool(options.ProxyCA)
	if err != nil {
		return err
	}

	if options.BaseBundle == nil {
		options.BaseBundle = DefaultBaseBundle()
	}

	base, err := Certificates(options.BaseBundle)
	if err != nil {
		return fmt.Errorf("base bundle: %w", err)
	}

	var extra []byte
	if len(strings.TrimSpace(string(options.ExtraCA))) > 0 {
		if extra, err = Certificates(options.ExtraCA); err != nil {
			return fmt.Errorf("extra CA: %w", err)
		}
	}

	ca, err := LoadOrCreateCA(options.StateDir, options.Now())
	if err != nil {
		return err
	}

	leaves, err := NewLeaves(ca, options.Now)
	if err != nil {
		return err
	}

	joined := newJoiner(options.TrustDir, ca, options.Joins)
	if err := joined.sync(); err != nil {
		return fmt.Errorf("write joined files: %w", err)
	}

	if err := WriteTrust(options.TrustDir, ca, base, extra); err != nil {
		return err
	}

	if options.JoinInterval <= 0 {
		options.JoinInterval = DefaultJoinInterval
	}

	joinCtx, stopJoining := context.WithCancel(ctx)
	joinDone := make(chan struct{})

	go func() {
		defer close(joinDone)
		joined.run(joinCtx, options.JoinInterval, options.Logger)
	}()

	// Run must not return while the joiner can still write into the trust directory.
	defer func() {
		stopJoining()
		<-joinDone
	}()

	server := &http.Server{
		Handler: &Forwarder{
			TokenFile: options.TokenFile,
			Transport: NewTransport(options.ProxyAddress, pool),
			Logger:    options.Logger,
		},
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig:         serverTLSConfig(leaves),
		ErrorLog:          slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
	}

	served := make(chan error, 1)

	go func() {
		served <- server.ServeTLS(progress.NewListener(options.Listener, progressTimeout), "", "")
	}()

	options.Logger.Info("interceptor started", "listen", options.Listener.Addr().String(), "proxy", options.ProxyAddress)

	select {
	case <-ctx.Done():
	case err := <-served:
		options.Logger.Error("interceptor stopped", "error", err)

		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err = server.Shutdown(shutdownCtx)

	options.Logger.Info("interceptor stopped")

	return err
}

// serverTLSConfig answers only the three GitHub hosts: a handshake with any
// other server name, or none, fails.
func serverTLSConfig(leaves *Leaves) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			switch strings.ToLower(hello.ServerName) {
			case HostAPI, HostUploads, HostGit:
				return leaves.Get()
			default:
				return nil, errors.New("the server name is not a GitHub host the interceptor serves")
			}
		},
	}
}

// Probe succeeds only when trustDir holds the ready file and, for a loopback
// address, a TLS dial to address with the server name api.github.com verifies
// against trustDir's ca.crt. A non-loopback address may not be on an interface
// yet (a KubeVirt guest gateway appears only after this probe passes), so it
// is not dialled: the ready file, written once the listener is bound and the
// leaf certificate exists, is the signal.
func Probe(ctx context.Context, address, trustDir string) error {
	if _, err := os.Stat(filepath.Join(trustDir, readyFile)); err != nil {
		return fmt.Errorf("the trust files are not ready: %w", err)
	}

	if !isLoopbackAddress(address) {
		return nil
	}

	caPEM, err := os.ReadFile(filepath.Join(trustDir, trustCAFile))
	if err != nil {
		return err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("the trust directory's ca.crt holds no certificate")
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: HostAPI, MinVersion: tls.VersionTLS12}}

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("the interceptor's certificate does not verify: %w", err)
	}

	return conn.Close()
}

// isLoopbackAddress reports whether address is a host:port whose host is a
// loopback IP. Anything else, including a name, counts as not loopback.
func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip.IsLoopback()
}
