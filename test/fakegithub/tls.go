package fakegithub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"time"
)

// GitHubHosts are the three names a proxied pod sends to the proxy, and that
// the fake serves in e2e: the REST and GraphQL API, release asset uploads and
// git over HTTPS.
var GitHubHosts = []string{"api.github.com", "uploads.github.com", "github.com"}

// StorageHost is where archive redirects point when the fake is served
// under GitHub's names, as GitHub redirects archives to codeload.github.com,
// a host the proxy doesn't serve. The fake's certificate names it too.
const StorageHost = "codeload.github.com"

// TestCA is a throwaway certificate authority that signs the fake's server
// certificate. Everything comes from the standard library.
type TestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// PEM is the CA certificate, to be trusted by clients under test.
	PEM []byte
}

// NewTestCA creates a CA valid for one day.
func NewTestCA() (*TestCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fakegithub test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &TestCA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// Pool is a certificate pool holding only this CA.
func (ca *TestCA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	return pool
}

// ServerCertificate issues a certificate for exactly the given DNS names.
func (ca *TestCA) ServerCertificate(hosts ...string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// TLSConfig serves the three GitHub host names with a certificate from the CA.
func (ca *TestCA) TLSConfig() (*tls.Config, error) {
	cert, err := ca.ServerCertificate(append(slices.Clone(GitHubHosts), StorageHost)...)
	if err != nil {
		return nil, err
	}

	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}
