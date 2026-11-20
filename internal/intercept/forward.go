package intercept

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// upstreamDialTimeout and upstreamHeaderTimeout bound connecting to the
	// proxy and waiting for its response headers, as the proxy does towards
	// GitHub.
	upstreamDialTimeout   = 10 * time.Second
	upstreamHeaderTimeout = 60 * time.Second

	codeMisdirected = "misdirected_request"
	codeUnavailable = "unavailable"

	// documentationURL is the user page refusals link to, as the proxy's do.
	documentationURL = "https://github.com/solidDoWant/githubgate/blob/master/docs/user/getting-access.md"
)

// canonicalHost returns the GitHub host a Host header names, with the
// proxy's rules: lower-cased, an optional ":443", no trailing dot, exactly
// one of the three hosts.
func canonicalHost(header string) (string, bool) {
	host := strings.TrimSuffix(strings.ToLower(header), ":443")

	switch host {
	case HostAPI, HostUploads, HostGit:
		return host, true
	default:
		return "", false
	}
}

// NewTransport returns the transport to the githubgate proxy. Whatever the
// request's host, it dials proxyAddress and verifies the proxy against
// proxyCA alone, with the GitHub host as the TLS server name. Proxy
// environment variables are ignored.
func NewTransport(proxyAddress string, proxyCA *x509.CertPool) *http.Transport {
	dialer := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}

	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, proxyAddress)
		},
		TLSClientConfig:       &tls.Config{RootCAs: proxyCA, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   upstreamDialTimeout,
		ResponseHeaderTimeout: upstreamHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}
}

// Forwarder sends each request for a GitHub host to the githubgate proxy
// with the pod's ServiceAccount token in place of any client credential.
type Forwarder struct {
	// TokenFile is read on every request, so a rotated token is used at once.
	TokenFile string
	// Transport reaches the proxy; see NewTransport.
	Transport http.RoundTripper
	// Logger receives errors, never headers; discarded when nil.
	Logger *slog.Logger
}

func (f *Forwarder) logger() *slog.Logger {
	if f.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}

	return f.Logger
}

// ServeHTTP checks the Host, reads the token and forwards the request.
func (f *Forwarder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	host, ok := canonicalHost(request.Host)
	if !ok {
		writeError(writer, request, "", http.StatusMisdirectedRequest, codeMisdirected,
			"the Host header must name api.github.com, uploads.github.com or github.com")

		return
	}

	token, err := readToken(f.TokenFile)
	if err != nil {
		f.logger().Error("reading the ServiceAccount token", "error", err)
		writeError(writer, request, host, http.StatusServiceUnavailable, codeUnavailable, "can't read the ServiceAccount token")

		return
	}

	proxy := &httputil.ReverseProxy{
		Transport:     f.Transport,
		FlushInterval: -1,
		Rewrite: func(out *httputil.ProxyRequest) {
			out.SetURL(&url.URL{Scheme: "https", Host: host})
			out.Out.Host = host

			out.Out.Header.Del("Authorization")
			out.Out.Header.Del("Proxy-Authorization")
			out.Out.Header.Del("Cookie")
			out.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			f.logger().Error("forwarding to the proxy", "host", host, "error", errorText(err))
			writeError(writer, request, host, http.StatusServiceUnavailable, codeUnavailable, "proxy unavailable")
		},
	}

	proxy.ServeHTTP(writer, request)
}

// errorText describes a forwarding error without the request URL, which
// may carry query parameters.
func errorText(err error) string {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err.Error()
	}

	return err.Error()
}

// readToken reads the ServiceAccount token file.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the ServiceAccount token file is empty")
	}

	return token, nil
}

// errorBody is the JSON body of an error on the API hosts, as the proxy
// writes it.
type errorBody struct {
	Message          string         `json:"message"`
	Error            string         `json:"error"`
	DocumentationURL string         `json:"documentation_url"`
	Errors           []graphQLError `json:"errors,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

// writeError answers as the proxy does: JSON on the API hosts (with
// GraphQL's errors list for GraphQL), text on github.com and on a host the
// interceptor doesn't serve.
func writeError(writer http.ResponseWriter, request *http.Request, host string, status int, code, message string) {
	message = "githubgate: " + message

	writer.Header().Set("Cache-Control", "no-store")

	if host != HostAPI && host != HostUploads {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(message + "\n"))

		return
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)

	body := errorBody{Message: message, Error: code, DocumentationURL: documentationURL}
	if host == HostAPI && request.Method == http.MethodPost && request.URL.Path == "/graphql" {
		body.Errors = []graphQLError{{Message: message}}
	}

	_ = json.NewEncoder(writer).Encode(body)
}
