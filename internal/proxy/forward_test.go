package proxy_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

const installationToken = "ghs_installationTokenForTests"

// upstreamRequest is what the fake upstream received.
type upstreamRequest struct {
	method string
	path   string
	query  string
	host   string
	header http.Header
	body   string
}

// fakeUpstream stands in for GitHub's three hosts, told apart by a path
// prefix, and records every request it gets.
type fakeUpstream struct {
	mu       sync.Mutex
	received []upstreamRequest
	respond  func(writer http.ResponseWriter, request *http.Request)
}

func (u *fakeUpstream) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)

	u.mu.Lock()
	u.received = append(u.received, upstreamRequest{
		method: request.Method, path: request.URL.EscapedPath(), query: request.URL.RawQuery,
		host: request.Host, header: request.Header.Clone(), body: string(body),
	})
	respond := u.respond
	u.mu.Unlock()

	if respond != nil {
		respond(writer, request)

		return
	}

	writer.Header().Set("Set-Cookie", "session=secret")
	writer.Header().Set("X-From-GitHub", "yes")
	_, _ = writer.Write([]byte(`{"name":"foo"}`))
}

func (u *fakeUpstream) requests() []upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]upstreamRequest(nil), u.received...)
}

// fakeTokens hands out installationToken and records the decisions it got.
type fakeTokens struct {
	mu        sync.Mutex
	decisions []proxy.Decision
	failure   error
}

func (f *fakeTokens) ProxyToken(_ context.Context, _ auth.Identity, decision proxy.Decision) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.decisions = append(f.decisions, decision)
	if f.failure != nil {
		return "", "github_refused", f.failure
	}

	return installationToken, "minted", nil
}

type forwardingProxy struct {
	handler  http.Handler
	upstream *fakeUpstream
	tokens   *fakeTokens
	audit    *bytes.Buffer
	server   *httptest.Server
}

func newForwardingProxy(t *testing.T, rules ...*model.Rule) *forwardingProxy {
	t.Helper()

	upstream := &fakeUpstream{}
	server := httptest.NewTLSServer(upstream)
	t.Cleanup(server.Close)

	base, err := url.Parse(server.URL)
	require.NoError(t, err)

	at := func(prefix string) *url.URL {
		copied := *base
		copied.Path = prefix

		return &copied
	}

	tokens := &fakeTokens{}
	audit := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{writer: audit}, nil))

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:     func(string, string) []*model.Rule { return rules },
		Tokens:    tokens,
		Upstreams: proxy.Upstreams{API: at("/api"), Uploads: at("/uploads"), Git: at("/git")},
		Transport: server.Client().Transport,
		Audit:     logger,
	})
	require.NoError(t, err)

	handler := proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder, Audit: logger})

	return &forwardingProxy{handler: handler, upstream: upstream, tokens: tokens, audit: audit, server: server}
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.writer.Write(data)
}

func (p *forwardingProxy) auditLines(t *testing.T) []map[string]any {
	t.Helper()

	lines := []map[string]any{}

	for _, line := range strings.Split(strings.TrimSpace(p.audit.String()), "\n") {
		if line == "" {
			continue
		}

		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &fields))
		lines = append(lines, fields)
	}

	return lines
}

func withBody(request *http.Request, body string) *http.Request {
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/json")

	return request
}

func TestProxyForwardsAllowedRequestsWithTheInstallationToken(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))

	request := authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo?per_page=5")
	request.Header.Set("Cookie", "user=1")
	request.Header.Set("X-Forwarded-For", "10.0.0.1")
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Port", "80")
	request.Header.Set("Accept", "application/vnd.github+json")

	response := serve(forwarding.handler, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"name":"foo"}`, response.Body.String())
	assert.Empty(t, response.Header().Values("Set-Cookie"), "GitHub's cookies reach the caller")
	assert.Equal(t, "yes", response.Header().Get("X-From-GitHub"))

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, "/api/repos/acme/foo", received[0].path)
	assert.Equal(t, "per_page=5", received[0].query)
	assert.Equal(t, strings.TrimPrefix(forwarding.server.URL, "https://"), received[0].host)
	assert.Equal(t, []string{"token " + installationToken}, received[0].header.Values("Authorization"))
	assert.Empty(t, received[0].header.Values("Cookie"))
	assert.Equal(t, "application/vnd.github+json", received[0].header.Get("Accept"))

	for name := range received[0].header {
		assert.False(t, strings.HasPrefix(name, "X-Forwarded-"), "%s was forwarded", name)
		assert.NotEqual(t, "Forwarded", name)
	}

	assert.NotContains(t, response.Body.String(), installationToken)
}

func TestProxyRefusesRequestsOutsideTheRulesWithoutForwarding(t *testing.T) {
	tests := []struct {
		name            string
		rules           []*model.Rule
		request         *http.Request
		expectedStatus  int
		expectedCode    string
		expectedMessage string
	}{
		{
			name:            "a repository no rule covers",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"))},
			request:         authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/bar"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"acme/bar"`,
		},
		{
			name:            "a denied operation",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), denyOperations("repos/create-release"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/releases"), `{"tag_name":"v1"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"repos/create-release"`,
		},
		{
			name:            "a release asset upload outside allowOperations",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), allowOperations("repos/get"))},
			request:         authenticated(http.MethodPost, proxy.HostUploads, "/repos/acme/foo/releases/1/assets?name=a.txt"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"repos/upload-release-asset"`,
		},
		{
			name:            "a denied mutation",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("pull_requests"), denyMutations("mergePullRequest"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/graphql"), `{"query":"mutation { mergePullRequest(input: {}) { clientMutationId } }"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"mergePullRequest"`,
		},
		{
			name:            "a ref created outside pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/git/refs"), `{"ref":"refs/heads/master","sha":"aa"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"refs/heads/master"`,
		},
		{
			name:            "a deployment that can auto-merge into a branch outside pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, contentsAndDeployments(), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/deployments"), `{"ref":"release","auto_merge":true,"required_contexts":[]}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"repos/create-deployment"`,
		},
		{
			name:            "a createDeployment mutation that can auto-merge under pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, contentsAndDeployments(), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/graphql"), `{"query":"mutation { createDeployment(input: {repositoryId: \"R\", refId: \"REF\", autoMerge: true}) { deployment { id } } }"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"createDeployment"`,
		},
		{
			name:            "publishing a draft release without tag_name under pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPatch, proxy.HostAPI, "/repos/acme/foo/releases/1"), `{"draft":false}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"repos/update-release"`,
		},
		{
			name:            "a release update with a draft value that isn't a boolean under pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPatch, proxy.HostAPI, "/repos/acme/foo/releases/1"), `{"draft":"false"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"repos/update-release"`,
		},
		{
			name:            "publishing a draft release with a tag outside pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPatch, proxy.HostAPI, "/repos/acme/foo/releases/1"), `{"draft":false,"tag_name":"v1"}`),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `"refs/tags/v1"`,
		},
		{
			name:            "a ref body with a key twice",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/git/refs"), `{"ref":"refs/heads/agent/x","ref":"refs/heads/master"}`),
			expectedStatus:  http.StatusBadRequest,
			expectedCode:    "invalid_request",
			expectedMessage: "JSON",
		},
		{
			name:           "a clone without contents read",
			rules:          []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("issues"))},
			request:        authenticated(http.MethodGet, proxy.HostGit, "/acme/foo.git/info/refs?service=git-upload-pack"),
			expectedStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, test.rules...)

			response := serve(forwarding.handler, test.request)

			require.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.Empty(t, forwarding.upstream.requests(), "the request was sent upstream")
			assert.Empty(t, forwarding.tokens.decisions, "a token was minted")

			if test.request.Host == proxy.HostGit {
				assert.Contains(t, response.Body.String(), "githubgate: ")

				return
			}

			var body struct {
				Message string `json:"message"`
				Error   string `json:"error"`
			}

			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, test.expectedCode, body.Error)
			assert.Contains(t, body.Message, test.expectedMessage)
		})
	}
}

func TestProxyForwardsEachHostToItsUpstream(t *testing.T) {
	rules := []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))}

	tests := []struct {
		name          string
		request       *http.Request
		expectedPath  string
		expectedAuth  string
		expectedBody  string
		expectedQuery string
	}{
		{
			name:          "a release asset upload goes to the uploads host",
			request:       withBody(authenticated(http.MethodPost, proxy.HostUploads, "/repos/acme/foo/releases/1/assets?name=a.txt"), "asset bytes"),
			expectedPath:  "/uploads/repos/acme/foo/releases/1/assets",
			expectedQuery: "name=a.txt",
			expectedAuth:  "token " + installationToken,
			expectedBody:  "asset bytes",
		},
		{
			name:          "git authenticates with the token as a Basic password",
			request:       authenticated(http.MethodGet, proxy.HostGit, "/acme/foo.git/info/refs?service=git-upload-pack"),
			expectedPath:  "/git/acme/foo.git/info/refs",
			expectedQuery: "service=git-upload-pack",
			expectedAuth:  "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+installationToken)),
		},
		{
			name:         "a ref inside pushRefs is created with the body as sent",
			request:      withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/git/refs"), `{"ref":"refs/heads/agent/x","sha":"aa"}`),
			expectedPath: "/api/repos/acme/foo/git/refs",
			expectedAuth: "token " + installationToken,
			expectedBody: `{"ref":"refs/heads/agent/x","sha":"aa"}`,
		},
		{
			name:         "a release update without tag_name that keeps it a draft is sent as is",
			request:      withBody(authenticated(http.MethodPatch, proxy.HostAPI, "/repos/acme/foo/releases/1"), `{"name":"notes","draft":true}`),
			expectedPath: "/api/repos/acme/foo/releases/1",
			expectedAuth: "token " + installationToken,
			expectedBody: `{"name":"notes","draft":true}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, rules...)

			response := serve(forwarding.handler, test.request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			received := forwarding.upstream.requests()
			require.Len(t, received, 1)
			assert.Equal(t, test.expectedPath, received[0].path)
			assert.Equal(t, test.expectedQuery, received[0].query)
			assert.Equal(t, []string{test.expectedAuth}, received[0].header.Values("Authorization"))
			assert.Equal(t, test.expectedBody, received[0].body)
		})
	}
}

func TestProxyForwardsTheClassifiedGraphQLDocument(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, write("pull_requests"), allowMutations("createPullRequest")))

	request := withBody(authenticated(http.MethodPost, proxy.HostAPI, "/graphql"),
		`{"query":"mutation   M { createPullRequest(input: {}) { clientMutationId } }","extensions":{"x":1}}`)

	response := serve(forwarding.handler, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, "/api/graphql", received[0].path)
	assert.NotContains(t, received[0].body, "extensions")
	assert.Contains(t, received[0].body, "createPullRequest")

	require.Len(t, forwarding.tokens.decisions, 1)
	assert.Equal(t, []string{"foo"}, forwarding.tokens.decisions[0].Repositories)
}

func TestProxyPassesRedirectsThroughUnchanged(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))
	forwarding.upstream.respond = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://codeload.github.com/acme/foo/legacy.tar.gz/main?token=short-lived")
		writer.WriteHeader(http.StatusFound)
	}

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo/tarball/main"))

	require.Equal(t, http.StatusFound, response.Code)
	assert.Equal(t, "https://codeload.github.com/acme/foo/legacy.tar.gz/main?token=short-lived", response.Header().Get("Location"))
	assert.Len(t, forwarding.upstream.requests(), 1)
}

func TestProxyAnswersUnavailableWhenGitHubRefusesTheToken(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))
	forwarding.upstream.respond = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"message":"Bad credentials"}`))
	}

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo"))

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), `"github_unavailable"`)
	assert.Len(t, forwarding.upstream.requests(), 1, "the request was retried")
}

func TestProxyPassesOnTokenFailures(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))
	forwarding.tokens.failure = &proxy.TokenError{Status: http.StatusForbidden, Code: "github_refused", Message: "GitHub refused the token"}

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo"))

	require.Equal(t, http.StatusForbidden, response.Code)
	assert.Contains(t, response.Body.String(), `"github_refused"`)
	assert.Empty(t, forwarding.upstream.requests())
}

func TestProxyWritesOneAuditLinePerRequest(t *testing.T) {
	forwarding := newForwardingProxy(t,
		proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"), denyMutations("mergePullRequest")))

	serve(forwarding.handler, withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/acme/foo/git/refs"), `{"ref":"refs/heads/agent/x","sha":"aa"}`))
	serve(forwarding.handler, withBody(authenticated(http.MethodPost, proxy.HostAPI, "/graphql"), `{"query":"mutation { mergePullRequest(input: {}) { clientMutationId } }"}`))
	serve(forwarding.handler, newRequest(http.MethodGet, proxy.HostAPI, "/repos/acme/foo"))

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 3)

	assert.Equal(t, "proxy", lines[0]["mode"])
	assert.Equal(t, "ci-a", lines[0]["namespace"])
	assert.Equal(t, "builder", lines[0]["service_account"])
	assert.Equal(t, "git/create-ref", lines[0]["operation"])
	assert.Equal(t, []any{"refs/heads/agent/x"}, lines[0]["refs"])
	assert.Equal(t, []any{"foo"}, lines[0]["repositories"])
	assert.Equal(t, "contents:write", lines[0]["permissions"])
	assert.Equal(t, "githubgate/app1-acme", lines[0]["installation"])
	assert.Equal(t, "allowed", lines[0]["decision"])
	assert.Equal(t, "200", lines[0]["github_status"])

	assert.Equal(t, []any{"mergePullRequest"}, lines[1]["mutations"])
	assert.Equal(t, "refused", lines[1]["decision"])
	assert.Equal(t, "denied", lines[1]["reason"])
	assert.Equal(t, "not_called", lines[1]["github_status"])

	assert.Equal(t, "refused", lines[2]["decision"])
	assert.Equal(t, "unauthenticated", lines[2]["reason"])

	assert.NotContains(t, forwarding.audit.String(), installationToken)
	assert.NotContains(t, forwarding.audit.String(), validToken)
}

// The formatter used to indent each nesting level, so a deeply nested
// document of a few KiB printed to hundreds of MiB before any rule was
// checked. Not parallel: the allocation count covers the whole process.
func TestProxyRefusesADeeplyNestedGraphQLDocumentWithoutLargeAllocations(t *testing.T) {
	forwarding := newForwardingProxy(t)

	deep := strings.Repeat("{a", 998) + strings.Repeat("}", 998)
	body := `{"query":"query {` + strings.Repeat(" a"+deep, 7) + `}"}`

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	response := serve(forwarding.handler, withBody(authenticated(http.MethodPost, proxy.HostAPI, "/graphql"), body))

	runtime.ReadMemStats(&after)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"denied"`)
	assert.Empty(t, forwarding.upstream.requests())

	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(400*len(body)), "allocated %d bytes for a %d-byte body", allocated, len(body))
}
