package proxy_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy"
)

// serverCertificate returns a throwaway certificate for names and a pool
// that trusts it.
func serverCertificate(t *testing.T, names ...string) (*tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "githubgate proxy test"},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	pool.AddCert(parsed)

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pool
}

func TestProxyServesCertificatesOnlyForTheGitHubHosts(t *testing.T) {
	// The certificate also names another host, so only the proxy's own check
	// can refuse it.
	certificate, pool := serverCertificate(t, proxy.HostAPI, proxy.HostUploads, proxy.HostGit, "other.example")

	listener, err := tls.Listen("tcp", "127.0.0.1:0", proxy.TLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return certificate, nil
	}))
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = connection.Close() }()

				_ = connection.(*tls.Conn).HandshakeContext(t.Context())
			}()
		}
	}()

	tests := []struct {
		serverName string
		accepted   bool
	}{
		{serverName: proxy.HostAPI, accepted: true},
		{serverName: proxy.HostUploads, accepted: true},
		{serverName: proxy.HostGit, accepted: true},
		{serverName: "API.GITHUB.COM", accepted: true},
		{serverName: "other.example"},
		{serverName: "gist.github.com"},
		{serverName: "api.github.com.other.example"},
		{serverName: ""},
	}

	for _, test := range tests {
		name := test.serverName
		if name == "" {
			name = "no server name"
		}

		t.Run(name, func(t *testing.T) {
			dialer := &tls.Dialer{
				NetDialer: &net.Dialer{Timeout: 10 * time.Second},
				Config: &tls.Config{
					MinVersion: tls.VersionTLS12,
					ServerName: test.serverName,
					RootCAs:    pool,
					// Without a server name the client sends no SNI; the
					// handshake must still fail on the server's side.
					InsecureSkipVerify: test.serverName == "", //nolint:gosec // only to send a ClientHello without SNI
				},
			}

			connection, err := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
			if test.accepted {
				require.NoError(t, err)
				assert.NoError(t, connection.Close())

				return
			}

			assert.Error(t, err)

			if connection != nil {
				_ = connection.Close()
			}
		})
	}
}
