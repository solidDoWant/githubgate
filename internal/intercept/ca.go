// Package intercept is the interceptor: a forwarder next to a workload that
// answers TLS for the GitHub hosts with a per-pod CA, replaces any client
// credential with the pod's ServiceAccount token, and sends the request to
// the githubgate proxy.
package intercept

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The GitHub hosts the interceptor answers for.
const (
	HostAPI     = "api.github.com"
	HostUploads = "uploads.github.com"
	HostGit     = "github.com"
)

// Hosts are the names on the leaf certificate and the CA's permitted DNS
// subtrees.
var Hosts = []string{HostAPI, HostUploads, HostGit}

const (
	caKeyFile  = "ca.key"
	caCertFile = "ca.crt"

	caLifetimeYears = 10
	leafLifetime    = 30 * 24 * time.Hour
	// renewBefore is how long before expiry a CA is replaced at start and a
	// leaf is re-issued.
	renewBefore = 7 * 24 * time.Hour
	// backdate keeps a certificate valid on a client whose clock is a little
	// behind.
	backdate = time.Hour
)

// CA is the per-pod certificate authority. Its key stays in memory and in
// the state directory.
type CA struct {
	certificate *x509.Certificate
	key         crypto.Signer
	// PEM is the CA certificate, PEM-encoded.
	PEM []byte
}

// LoadOrCreateCA returns the CA in stateDir when ca.key and ca.crt parse,
// belong together and have more than seven days left. Otherwise it creates
// a new CA and writes both files there with mode 0600.
func LoadOrCreateCA(stateDir string, now time.Time) (*CA, error) {
	ca, err := loadCA(stateDir, now)
	if err == nil {
		return ca, nil
	}

	if ca, err = newCA(now); err != nil {
		return nil, err
	}

	if err := ca.save(stateDir); err != nil {
		return nil, err
	}

	return ca, nil
}

func loadCA(stateDir string, now time.Time) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(stateDir, caCertFile))
	if err != nil {
		return nil, err
	}

	keyPEM, err := os.ReadFile(filepath.Join(stateDir, caKeyFile))
	if err != nil {
		return nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)

	if certBlock == nil || certBlock.Type != "CERTIFICATE" || keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, errors.New("the stored CA is not PEM")
	}

	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}

	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, errors.New("the stored CA key does not parse")
	}

	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(certificate.PublicKey) {
		return nil, errors.New("the stored CA key does not match its certificate")
	}

	if !certificate.IsCA || !certificate.NotAfter.After(now.Add(renewBefore)) {
		return nil, errors.New("the stored CA is not usable")
	}

	return &CA{certificate: certificate, key: key, PEM: pem.EncodeToMemory(certBlock)}, nil
}

func newCA(now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	_, allIPv4, _ := net.ParseCIDR("0.0.0.0/0")
	_, allIPv6, _ := net.ParseCIDR("::/0")

	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "githubgate interceptor CA"},
		NotBefore:                   now.Add(-backdate),
		NotAfter:                    now.AddDate(caLifetimeYears, 0, 0),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         append([]string(nil), Hosts...),
		// An empty permitted IP list would leave IP addresses unconstrained,
		// so every address is excluded instead.
		ExcludedIPRanges: []*net.IPNet{allIPv4, allIPv6},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, err
	}

	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &CA{
		certificate: certificate,
		key:         key,
		PEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// save writes the CA's key and certificate to stateDir, each with mode
// 0600 and by write-and-rename.
func (c *CA) save(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if err := writeFileAtomic(filepath.Join(stateDir, caKeyFile), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write the CA key: %w", err)
	}

	if err := writeFileAtomic(filepath.Join(stateDir, caCertFile), c.PEM, 0o600); err != nil {
		return fmt.Errorf("write the CA certificate: %w", err)
	}

	return nil
}

// Certificate returns the parsed CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.certificate }

// IssueLeaf returns a new key and a certificate for exactly the three
// GitHub hosts, valid for thirty days.
func (c *CA) IssueLeaf(now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: HostAPI},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     append([]string(nil), Hosts...),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.certificate, key.Public(), c.key)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Leaves holds the current leaf in memory and re-issues it seven days
// before it expires.
type Leaves struct {
	ca  *CA
	now func() time.Time

	mu      sync.Mutex
	current *tls.Certificate
}

// NewLeaves returns a holder that issues its first leaf at once. now is the
// clock; time.Now when nil.
func NewLeaves(ca *CA, now func() time.Time) (*Leaves, error) {
	if now == nil {
		now = time.Now
	}

	leaves := &Leaves{ca: ca, now: now}
	if _, err := leaves.Get(); err != nil {
		return nil, err
	}

	return leaves, nil
}

// Get returns the current leaf, issuing a new one when it has seven days
// or less left.
func (l *Leaves) Get() (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.current != nil && l.current.Leaf.NotAfter.After(now.Add(renewBefore)) {
		return l.current, nil
	}

	leaf, err := l.ca.IssueLeaf(now)
	if err != nil {
		return nil, err
	}

	l.current = leaf

	return leaf, nil
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// writeFileAtomic writes data to a temporary file next to path with mode,
// then renames it over path, so readers see the old file or the new one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}

	name := file.Name()
	defer os.Remove(name) //nolint:errcheck // gone after a successful rename

	if err := file.Chmod(mode); err != nil {
		_ = file.Close()

		return err
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(name, path)
}
