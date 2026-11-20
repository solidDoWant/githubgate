package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultURL       = "https://githubgate.githubgate.svc:8443"
	defaultCAFile    = "/var/run/secrets/githubgate/ca.crt"
	defaultTokenFile = "/var/run/secrets/githubgate/token"

	maxResponseBytes = 1 << 20
	requestTimeout   = 30 * time.Second
	retryMinMillis   = 200
	retryMaxMillis   = 500
)

// tokenRequest is the body of POST /v1/token.
type tokenRequest struct {
	Installation string            `json:"installation,omitempty"`
	Owner        string            `json:"owner,omitempty"`
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// tokenResponse is the success body of POST /v1/token.
type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// refusalError is an error answer from githubgate. Its message is githubgate's
// own text, which never contains a token.
type refusalError struct {
	status  int
	code    string
	message string
}

func (e *refusalError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("githubgate answered %d %s", e.status, e.code)
	}

	return fmt.Sprintf("githubgate refused the request (%s): %s", e.code, e.message)
}

// requestToken asks githubgate for a token. It retries once, after 200-500 ms,
// on a connection error or a 503 not_ready answer.
func requestToken(ctx context.Context, env *environment, request tokenRequest) (*tokenResponse, error) {
	endpoint, httpClient, err := newHTTPClient(env)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	response, retryable, err := postToken(ctx, httpClient, env, endpoint, body)
	if err == nil || !retryable {
		return response, err
	}

	delay := retryMinMillis + rand.IntN(retryMaxMillis-retryMinMillis+1)
	if sleepErr := env.sleep(ctx, delay); sleepErr != nil {
		return nil, sleepErr
	}

	response, _, err = postToken(ctx, httpClient, env, endpoint, body)

	return response, err
}

func newHTTPClient(env *environment) (string, *http.Client, error) {
	raw := env.getenv("GITHUBGATE_URL")
	if raw == "" {
		raw = defaultURL
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", nil, fmt.Errorf("GITHUBGATE_URL %q is not a valid URL", raw)
	}

	if parsed.Scheme != "https" {
		return "", nil, errors.New("GITHUBGATE_URL must use https: an http URL would send the ServiceAccount token in clear text")
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	caFile := env.getenv("GITHUBGATE_CA_FILE")
	explicitCA := caFile != ""

	if !explicitCA {
		caFile = defaultCAFile
	}

	pem, err := os.ReadFile(caFile)

	switch {
	case err == nil:
		if !pool.AppendCertsFromPEM(pem) {
			return "", nil, fmt.Errorf("CA file %s holds no certificate", caFile)
		}
	case explicitCA || !errors.Is(err, os.ErrNotExist):
		return "", nil, fmt.Errorf("read CA file: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		// A redirect could carry the ServiceAccount token elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return strings.TrimSuffix(parsed.String(), "/") + "/v1/token", httpClient, nil
}

func postToken(ctx context.Context, httpClient *http.Client, env *environment, endpoint string, body []byte) (*tokenResponse, bool, error) {
	tokenFile := env.getenv("GITHUBGATE_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = defaultTokenFile
	}

	// Read on every call so a rotated token is picked up.
	saToken, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, false, fmt.Errorf("read ServiceAccount token file: %w", err)
	}

	//nolint:forbidigo // This request goes to githubgate, never to GitHub.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(saToken)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		// The transport error can echo the URL but never the headers.
		return nil, ctx.Err() == nil, fmt.Errorf("cannot reach githubgate: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, true, fmt.Errorf("read githubgate response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var out tokenResponse
		if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
			return nil, false, errors.New("githubgate returned an unreadable token response")
		}

		return &out, false, nil
	}

	refusal := &refusalError{status: resp.StatusCode}

	var errBody struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &errBody) == nil {
		refusal.code = errBody.Error
		refusal.message = errBody.Message
	}

	return nil, resp.StatusCode == http.StatusServiceUnavailable && refusal.code == "not_ready", refusal
}

func sleepContext(ctx context.Context, millis int) error {
	timer := time.NewTimer(time.Duration(millis) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
