package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const defaultProxyCAFile = "/var/run/secrets/githubgate-proxy/ca.crt"

// proxyCA returns the path of the proxy CA file and a pool holding only its
// certificates. The ServiceAccount token goes to the proxy in place of a
// GitHub token, so a client in proxy mode trusts nothing else: a request that
// reaches the real GitHub by mistake fails TLS instead of sending the token.
func proxyCA(env *environment) (string, *x509.CertPool, error) {
	path := env.getenv("GITHUBGATE_PROXY_CA_FILE")
	if path == "" {
		path = defaultProxyCAFile
	}

	pem, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read the proxy CA file: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return "", nil, fmt.Errorf("proxy CA file %s holds no certificate", path)
	}

	return path, pool, nil
}

// proxyHTTPClient is an HTTP client that trusts only the proxy CA and never
// follows a redirect, which could carry the ServiceAccount token elsewhere.
func proxyHTTPClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// serviceAccountToken reads the ServiceAccount token file. It is read on every
// call so a rotated token is picked up.
func serviceAccountToken(env *environment) (string, error) {
	path := env.getenv("GITHUBGATE_TOKEN_FILE")
	if path == "" {
		path = defaultTokenFile
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read ServiceAccount token file: %w", err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the ServiceAccount token file is empty")
	}

	return token, nil
}
