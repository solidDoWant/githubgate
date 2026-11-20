package proxy

import (
	"crypto/tls"
	"errors"
	"strings"
)

// errUnservedName is the handshake error for a server name the proxy has no
// certificate for.
var errUnservedName = errors.New("the proxy serves only api.github.com, uploads.github.com and github.com")

// TLSConfig returns the proxy listener's TLS configuration: getCertificate
// (for example a certwatcher's, so a rotated certificate is used by new
// handshakes) answers only for the three GitHub hosts; a ClientHello for
// any other name, or none, fails the handshake.
func TLSConfig(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			switch strings.ToLower(hello.ServerName) {
			case HostAPI, HostUploads, HostGit:
				return getCertificate(hello)
			default:
				return nil, errUnservedName
			}
		},
	}
}
