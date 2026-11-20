package intercept

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

const (
	trustCAFile     = "ca.crt"
	trustBundleFile = "bundle.crt"
	readyFile       = "ready"
	trustFileMode   = 0o444
)

// DefaultBaseBundle returns the roots of golang.org/x/crypto's fallback
// bundle as PEM. Roots that carry an extra constraint (for example a
// distrust date) are left out: a PEM file can't carry the constraint, and
// trusting them without it would trust more than Go does.
func DefaultBaseBundle() []byte {
	var out bytes.Buffer

	for root := range bundle.Roots() {
		if root.Constraint != nil {
			continue
		}

		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: root.Certificate})
	}

	return out.Bytes()
}

// Certificates returns the certificates in data, PEM-encoded again. It
// fails on a block that is not a certificate, on a certificate that does
// not parse, and when data holds none, so the trust files hold only
// certificates.
func Certificates(data []byte) ([]byte, error) {
	var out bytes.Buffer

	for {
		var block *pem.Block

		block, data = pem.Decode(data)
		if block == nil {
			break
		}

		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a %q PEM block is not a certificate", block.Type)
		}

		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("a certificate does not parse: %w", err)
		}

		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	}

	if out.Len() == 0 {
		return nil, errors.New("no certificate found")
	}

	return out.Bytes(), nil
}

// WriteTrust writes ca.crt (the pod CA only) and bundle.crt (base, then
// extra, then the pod CA) to dir with mode 0444, each by write-and-rename,
// and then the ready file. base and extra must already hold only
// certificates; extra may be empty.
func WriteTrust(dir string, ca *CA, base, extra []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the workload reads this directory
		return fmt.Errorf("create the trust directory: %w", err)
	}

	if err := writeFileAtomic(filepath.Join(dir, trustCAFile), ca.PEM, trustFileMode); err != nil {
		return fmt.Errorf("write %s: %w", trustCAFile, err)
	}

	bundled := make([]byte, 0, len(base)+len(extra)+len(ca.PEM))
	bundled = append(bundled, base...)
	bundled = append(bundled, extra...)
	bundled = append(bundled, ca.PEM...)

	if err := writeFileAtomic(filepath.Join(dir, trustBundleFile), bundled, trustFileMode); err != nil {
		return fmt.Errorf("write %s: %w", trustBundleFile, err)
	}

	if err := writeFileAtomic(filepath.Join(dir, readyFile), nil, trustFileMode); err != nil {
		return fmt.Errorf("write %s: %w", readyFile, err)
	}

	return nil
}
