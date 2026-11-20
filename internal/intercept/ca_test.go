package intercept

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var nameConstraintsOID = asn1.ObjectIdentifier{2, 5, 29, 30}

func TestNewCAIsAConstrainedP256CA(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	ca, err := LoadOrCreateCA(t.TempDir(), now)
	require.NoError(t, err)

	certificate := ca.Certificate()

	assert.True(t, certificate.IsCA)
	assert.True(t, certificate.BasicConstraintsValid)
	assert.Equal(t, 0, certificate.MaxPathLen)
	assert.True(t, certificate.MaxPathLenZero)
	assert.Equal(t, now.Add(-time.Hour), certificate.NotBefore)
	assert.Equal(t, time.Date(2036, 11, 20, 12, 0, 0, 0, time.UTC), certificate.NotAfter)

	assert.True(t, certificate.PermittedDNSDomainsCritical)
	assert.ElementsMatch(t, []string{"api.github.com", "uploads.github.com", "github.com"}, certificate.PermittedDNSDomains)
	assert.Empty(t, certificate.PermittedIPRanges)

	excluded := make([]string, 0, len(certificate.ExcludedIPRanges))
	for _, network := range certificate.ExcludedIPRanges {
		excluded = append(excluded, network.String())
	}

	assert.ElementsMatch(t, []string{"0.0.0.0/0", "::/0"}, excluded)

	found := false

	for _, extension := range certificate.Extensions {
		if extension.Id.Equal(nameConstraintsOID) {
			found = true

			assert.True(t, extension.Critical, "the name constraints extension must be critical")
		}
	}

	assert.True(t, found, "the CA has a name constraints extension")

	key, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	assert.Equal(t, elliptic.P256(), key.Curve)
}

func TestCAFilesAreWrittenOnlyToTheStateDirectoryWithMode0600(t *testing.T) {
	stateDir := t.TempDir()

	_, err := LoadOrCreateCA(stateDir, time.Now())
	require.NoError(t, err)

	entries, err := os.ReadDir(stateDir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())

		info, err := entry.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), entry.Name())
	}

	assert.ElementsMatch(t, []string{"ca.key", "ca.crt"}, names)

	keyPEM, err := os.ReadFile(filepath.Join(stateDir, "ca.key"))
	require.NoError(t, err)

	block, _ := pem.Decode(keyPEM)
	require.NotNil(t, block)
	assert.Equal(t, "PRIVATE KEY", block.Type)
}

func TestCAIsReusedOnlyWithMoreThanSevenDaysLeft(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		after  func(notAfter time.Time) time.Time
		reused bool
	}{
		{name: "right away", after: func(time.Time) time.Time { return created.Add(time.Minute) }, reused: true},
		{name: "seven days and a second left", after: func(notAfter time.Time) time.Time { return notAfter.Add(-7*24*time.Hour - time.Second) }, reused: true},
		{name: "exactly seven days left", after: func(notAfter time.Time) time.Time { return notAfter.Add(-7 * 24 * time.Hour) }, reused: false},
		{name: "expired", after: func(notAfter time.Time) time.Time { return notAfter.Add(time.Hour) }, reused: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()

			first, err := LoadOrCreateCA(stateDir, created)
			require.NoError(t, err)

			second, err := LoadOrCreateCA(stateDir, test.after(first.Certificate().NotAfter))
			require.NoError(t, err)

			assert.Equal(t, test.reused, string(first.PEM) == string(second.PEM))

			stored, err := os.ReadFile(filepath.Join(stateDir, "ca.crt"))
			require.NoError(t, err)
			assert.Equal(t, string(second.PEM), string(stored), "the state directory holds the CA in use")
		})
	}
}

func TestCAIsReplacedWhenTheStoredFilesAreUnusable(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, stateDir string)
	}{
		{name: "key missing", damage: func(t *testing.T, stateDir string) {
			require.NoError(t, os.Remove(filepath.Join(stateDir, "ca.key")))
		}},
		{name: "certificate garbled", damage: func(t *testing.T, stateDir string) {
			require.NoError(t, os.WriteFile(filepath.Join(stateDir, "ca.crt"), []byte("not pem"), 0o600))
		}},
		{name: "key from another CA", damage: func(t *testing.T, stateDir string) {
			other := t.TempDir()
			_, err := LoadOrCreateCA(other, time.Now())
			require.NoError(t, err)

			key, err := os.ReadFile(filepath.Join(other, "ca.key"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(stateDir, "ca.key"), key, 0o600))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()

			first, err := LoadOrCreateCA(stateDir, time.Now())
			require.NoError(t, err)

			test.damage(t, stateDir)

			second, err := LoadOrCreateCA(stateDir, time.Now())
			require.NoError(t, err)
			assert.NotEqual(t, string(first.PEM), string(second.PEM))
		})
	}
}

func TestLeafNamesExactlyTheThreeHostsForThirtyDays(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	ca, err := LoadOrCreateCA(t.TempDir(), now)
	require.NoError(t, err)

	leaf, err := ca.IssueLeaf(now)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"api.github.com", "uploads.github.com", "github.com"}, leaf.Leaf.DNSNames)
	assert.Empty(t, leaf.Leaf.IPAddresses)
	assert.Empty(t, leaf.Leaf.URIs)
	assert.Empty(t, leaf.Leaf.EmailAddresses)
	assert.False(t, leaf.Leaf.IsCA)
	assert.Equal(t, now.Add(30*24*time.Hour), leaf.Leaf.NotAfter)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())

	for _, host := range []string{"api.github.com", "uploads.github.com", "github.com"} {
		_, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: now})
		assert.NoError(t, err, host)
	}

	_, err = leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "gist.github.com", CurrentTime: now})
	assert.Error(t, err)
}

func TestCAConstraintsRejectLeavesOutsideTheGitHubHosts(t *testing.T) {
	now := time.Now()

	ca, err := LoadOrCreateCA(t.TempDir(), now)
	require.NoError(t, err)

	tests := []struct {
		name     string
		dnsNames []string
		ips      []net.IP
	}{
		{name: "IPv4 address", dnsNames: []string{"api.github.com"}, ips: []net.IP{net.ParseIP("127.0.0.1")}},
		{name: "IPv6 address", dnsNames: []string{"api.github.com"}, ips: []net.IP{net.ParseIP("::1")}},
		{name: "another domain", dnsNames: []string{"example.com"}},
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			require.NoError(t, err)

			template := &x509.Certificate{
				SerialNumber: big.NewInt(42),
				NotBefore:    now.Add(-time.Hour),
				NotAfter:     now.Add(time.Hour),
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				DNSNames:     test.dnsNames,
				IPAddresses:  test.ips,
			}

			der, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate(), key.Public(), ca.key)
			require.NoError(t, err)

			leaf, err := x509.ParseCertificate(der)
			require.NoError(t, err)

			_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: test.dnsNames[0], CurrentTime: now})
			assert.Error(t, err)
		})
	}
}

func TestLeafIsReissuedSevenDaysBeforeExpiry(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := start

	ca, err := LoadOrCreateCA(t.TempDir(), start)
	require.NoError(t, err)

	leaves, err := NewLeaves(ca, func() time.Time { return now })
	require.NoError(t, err)

	first, err := leaves.Get()
	require.NoError(t, err)

	now = start.Add(23*24*time.Hour - time.Second)
	same, err := leaves.Get()
	require.NoError(t, err)
	assert.Same(t, first, same, "more than seven days left")

	now = start.Add(23 * 24 * time.Hour)
	renewed, err := leaves.Get()
	require.NoError(t, err)
	assert.NotSame(t, first, renewed, "seven days left")
	assert.Equal(t, now.Add(30*24*time.Hour), renewed.Leaf.NotAfter)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	_, err = renewed.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "github.com", CurrentTime: now})
	assert.NoError(t, err, "the same CA signs the new leaf")
}
