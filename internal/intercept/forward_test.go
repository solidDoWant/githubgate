package intercept

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seenRequest is what the stub proxy received.
type seenRequest struct {
	Host       string
	ServerName string
	Path       string
	Query      string
	Proto      int
	Header     http.Header
}

// stubProxy is a TLS server standing in for the githubgate proxy, with a
// certificate for the GitHub hosts from its own CA.
type stubProxy struct {
	server *httptest.Server
	caPEM  []byte

	mu   sync.Mutex
	seen []seenRequest
}

func newStubProxy(t *testing.T, handler http.HandlerFunc) *stubProxy {
	t.Helper()

	ca, err := newCA(time.Now())
	require.NoError(t, err)

	leaf, err := ca.IssueLeaf(time.Now())
	require.NoError(t, err)

	stub := &stubProxy{caPEM: ca.PEM}
	stub.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stub.mu.Lock()
		stub.seen = append(stub.seen, seenRequest{
			Host:       request.Host,
			ServerName: request.TLS.ServerName,
			Path:       request.URL.Path,
			Query:      request.URL.RawQuery,
			Proto:      request.ProtoMajor,
			Header:     request.Header.Clone(),
		})
		stub.mu.Unlock()

		if handler != nil {
			handler(writer, request)

			return
		}

		_, _ = io.WriteString(writer, "from the proxy")
	}))
	stub.server.EnableHTTP2 = true
	stub.server.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS12}
	stub.server.StartTLS()
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *stubProxy) address() string { return s.server.Listener.Addr().String() }

func (s *stubProxy) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]seenRequest(nil), s.seen...)
}

func (s *stubProxy) pool(t *testing.T) *x509.CertPool {
	t.Helper()

	pool, err := ProxyCAPool(s.caPEM)
	require.NoError(t, err)

	return pool
}

func writeToken(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token), 0o600))

	return path
}

// forward sends request through a Forwarder to stub and returns the answer.
func forward(t *testing.T, forwarder *Forwarder, request *http.Request) *http.Response {
	t.Helper()

	recorder := httptest.NewRecorder()
	forwarder.ServeHTTP(recorder, request)

	return recorder.Result()
}

func TestForwarderReplacesEveryClientCredentialWithTheServiceAccountToken(t *testing.T) {
	stub := newStubProxy(t, nil)
	forwarder := &Forwarder{TokenFile: writeToken(t, "sa-token\n"), Transport: NewTransport(stub.address(), stub.pool(t))}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/acme/foo?per_page=1", nil)
	request.Header.Add("Authorization", "token ghp_personal")
	request.Header.Add("Authorization", "Basic eC1hY2Nlc3MtdG9rZW46Z2hwX3BlcnNvbmFs")
	request.Header.Set("Proxy-Authorization", "Basic cHJveHk6c2VjcmV0")
	request.Header.Set("Cookie", "user_session=secret")
	request.Header.Set("Accept", "application/vnd.github+json")

	response := forward(t, forwarder, request)
	require.Equal(t, http.StatusOK, response.StatusCode)

	seen := stub.requests()
	require.Len(t, seen, 1)
	assert.Equal(t, []string{"Bearer sa-token"}, seen[0].Header.Values("Authorization"))
	assert.Empty(t, seen[0].Header.Values("Proxy-Authorization"))
	assert.Empty(t, seen[0].Header.Values("Cookie"))
	assert.Equal(t, "application/vnd.github+json", seen[0].Header.Get("Accept"))

	for name, values := range seen[0].Header {
		for _, value := range values {
			assert.NotContains(t, value, "ghp_personal", name)
			assert.NotContains(t, value, "secret", name)
			assert.NotContains(t, value, "eC1hY2Nlc3MtdG9rZW46Z2hwX3BlcnNvbmFs", name)
		}
	}
}

func TestForwarderSendsToTheProxyAsTheCanonicalGitHubHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "api", host: "api.github.com", want: "api.github.com"},
		{name: "upper case with port 443", host: "API.GitHub.com:443", want: "api.github.com"},
		{name: "uploads", host: "uploads.github.com", want: "uploads.github.com"},
		{name: "git", host: "github.com", want: "github.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newStubProxy(t, nil)
			forwarder := &Forwarder{TokenFile: writeToken(t, "sa-token"), Transport: NewTransport(stub.address(), stub.pool(t))}

			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid/acme/foo.git/info/refs?service=git-upload-pack", nil)
			request.Host = test.host

			response := forward(t, forwarder, request)
			require.Equal(t, http.StatusOK, response.StatusCode)

			seen := stub.requests()
			require.Len(t, seen, 1)
			assert.Equal(t, test.want, seen[0].Host)
			assert.Equal(t, test.want, seen[0].ServerName)
			assert.Equal(t, "/acme/foo.git/info/refs", seen[0].Path)
			assert.Equal(t, "service=git-upload-pack", seen[0].Query)
		})
	}
}

func TestTransportIgnoresProxyEnvironmentVariables(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	stub := newStubProxy(t, nil)
	forwarder := &Forwarder{TokenFile: writeToken(t, "sa-token"), Transport: NewTransport(stub.address(), stub.pool(t))}

	response := forward(t, forwarder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/user", nil))
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Len(t, stub.requests(), 1)
}

func TestTransportHasTheProxyTimeouts(t *testing.T) {
	transport := NewTransport("127.0.0.1:1", x509.NewCertPool())

	assert.Nil(t, transport.Proxy)
	assert.Equal(t, 60*time.Second, transport.ResponseHeaderTimeout)
	assert.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	assert.True(t, transport.ForceAttemptHTTP2)
}

func TestForwarderStreamsTheResponseBackUnchanged(t *testing.T) {
	stub := newStubProxy(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://objects.githubusercontent.com/release-asset?sig=abc")
		writer.Header().Set("X-GitHub-Request-Id", "1234")
		writer.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(writer, "redirecting")
	})
	forwarder := &Forwarder{TokenFile: writeToken(t, "sa-token"), Transport: NewTransport(stub.address(), stub.pool(t))}

	response := forward(t, forwarder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/acme/foo/releases/assets/1", nil))

	assert.Equal(t, http.StatusFound, response.StatusCode)
	assert.Equal(t, "https://objects.githubusercontent.com/release-asset?sig=abc", response.Header.Get("Location"))
	assert.Equal(t, "1234", response.Header.Get("X-GitHub-Request-Id"))

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "redirecting", string(body))
}

func TestForwarderErrors(t *testing.T) {
	missingToken := func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }
	emptyToken := func(t *testing.T) string { return writeToken(t, " \n") }
	unreadableToken := func(t *testing.T) string { return t.TempDir() }
	goodToken := func(t *testing.T) string { return writeToken(t, "sa-token") }

	tests := []struct {
		name        string
		tokenFile   func(t *testing.T) string
		proxy       func(t *testing.T, stub *stubProxy) (string, *x509.CertPool)
		method      string
		host        string
		path        string
		status      int
		message     string
		code        string
		graphQL     bool
		reachesStub bool
	}{
		{name: "token file missing on the API host", tokenFile: missingToken, host: "api.github.com",
			status: http.StatusServiceUnavailable, message: "githubgate: can't read the ServiceAccount token", code: "unavailable"},
		{name: "token file empty on uploads", tokenFile: emptyToken, host: "uploads.github.com",
			status: http.StatusServiceUnavailable, message: "githubgate: can't read the ServiceAccount token", code: "unavailable"},
		{name: "token file unreadable on github.com", tokenFile: unreadableToken, host: "github.com",
			status: http.StatusServiceUnavailable, message: "githubgate: can't read the ServiceAccount token"},
		{name: "token file missing on GraphQL", tokenFile: missingToken, host: "api.github.com", method: http.MethodPost, path: "/graphql",
			status: http.StatusServiceUnavailable, message: "githubgate: can't read the ServiceAccount token", code: "unavailable", graphQL: true},
		{name: "proxy unreachable", tokenFile: goodToken, host: "api.github.com",
			proxy: func(t *testing.T, stub *stubProxy) (string, *x509.CertPool) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)

				address := listener.Addr().String()
				require.NoError(t, listener.Close())

				return address, stub.pool(t)
			},
			status: http.StatusServiceUnavailable, message: "githubgate: proxy unavailable", code: "unavailable"},
		{name: "proxy certificate from another CA", tokenFile: goodToken, host: "github.com",
			proxy: func(t *testing.T, stub *stubProxy) (string, *x509.CertPool) {
				other, err := newCA(time.Now())
				require.NoError(t, err)

				pool, err := ProxyCAPool(other.PEM)
				require.NoError(t, err)

				return stub.address(), pool
			},
			status: http.StatusServiceUnavailable, message: "githubgate: proxy unavailable"},
		{name: "Host not a GitHub host", tokenFile: goodToken, host: "example.com",
			status: http.StatusMisdirectedRequest, message: "githubgate: the Host header must name api.github.com, uploads.github.com or github.com"},
		{name: "Host with a trailing dot", tokenFile: goodToken, host: "api.github.com.",
			status: http.StatusMisdirectedRequest, message: "githubgate: the Host header must name api.github.com, uploads.github.com or github.com"},
		{name: "Host a subdomain", tokenFile: goodToken, host: "gist.github.com",
			status: http.StatusMisdirectedRequest, message: "githubgate: the Host header must name api.github.com, uploads.github.com or github.com"},
		{name: "Host with another port", tokenFile: goodToken, host: "api.github.com:8443",
			status: http.StatusMisdirectedRequest, message: "githubgate: the Host header must name api.github.com, uploads.github.com or github.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := newStubProxy(t, nil)

			address, pool := stub.address(), stub.pool(t)
			if test.proxy != nil {
				address, pool = test.proxy(t, stub)
			}

			forwarder := &Forwarder{TokenFile: test.tokenFile(t), Transport: NewTransport(address, pool)}

			method, path := test.method, test.path
			if method == "" {
				method, path = http.MethodGet, "/repos/acme/foo"
			}

			request := httptest.NewRequestWithContext(t.Context(), method, "https://placeholder"+path, strings.NewReader("{}"))
			request.Host = test.host
			request.Header.Set("Authorization", "token ghp_personal")

			response := forward(t, forwarder, request)
			assert.Equal(t, test.status, response.StatusCode)
			assert.Empty(t, stub.requests(), "nothing reaches the proxy")

			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			if test.code == "" {
				assert.Equal(t, "text/plain; charset=utf-8", response.Header.Get("Content-Type"))
				assert.Equal(t, test.message+"\n", string(body))

				return
			}

			assert.Equal(t, "application/json; charset=utf-8", response.Header.Get("Content-Type"))

			var decoded struct {
				Message          string `json:"message"`
				Error            string `json:"error"`
				DocumentationURL string `json:"documentation_url"`
				Errors           []struct {
					Message string `json:"message"`
				} `json:"errors"`
			}
			require.NoError(t, json.Unmarshal(body, &decoded))
			assert.Equal(t, test.message, decoded.Message)
			assert.Equal(t, test.code, decoded.Error)
			assert.NotEmpty(t, decoded.DocumentationURL)

			if test.graphQL {
				require.Len(t, decoded.Errors, 1)
				assert.Equal(t, test.message, decoded.Errors[0].Message)
			} else {
				assert.Empty(t, decoded.Errors)
			}
		})
	}
}
