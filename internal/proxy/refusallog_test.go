package proxy_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy"
)

func TestProxyLogsTheTargetOfARefusedRequest(t *testing.T) {
	tests := []struct {
		name    string
		request *http.Request
		reason  string
		host    string
		method  string
		path    string
		query   []any
	}{
		{
			name:    "a path that matches no operation",
			request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo/no-such-thing?page=2&per_page=100&page=3"),
			reason:  "unknown_operation",
			host:    proxy.HostAPI,
			method:  http.MethodGet,
			path:    "/repos/acme/foo/no-such-thing",
			query:   []any{"page", "per_page"},
		},
		{
			name:    "a path the proxy does not accept",
			request: authenticated(http.MethodDelete, proxy.HostAPI, "/repos/acme/foo/git/refs/heads//x"),
			reason:  "invalid_request",
			host:    proxy.HostAPI,
			method:  http.MethodDelete,
			path:    "/repos/acme/foo/git/refs/heads//x",
			query:   []any{},
		},
		{
			name:    "a web path no rule turns on",
			request: authenticated(http.MethodGet, proxy.HostGit, "/acme/foo?go-get=1"),
			reason:  "not_found",
			host:    proxy.HostGit,
			method:  http.MethodGet,
			path:    "/acme/foo",
			query:   []any{"go-get"},
		},
		{
			name:    "a request policy denies",
			request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/bar"),
			reason:  "denied",
			host:    proxy.HostAPI,
			method:  http.MethodGet,
			path:    "/repos/acme/bar",
			query:   []any{},
		},
		{
			name:    "a request without a credential",
			request: newRequest(http.MethodGet, proxy.HostAPI, "/repos/acme/foo?ref=main"),
			reason:  "unauthenticated",
			host:    proxy.HostAPI,
			method:  http.MethodGet,
			path:    "/repos/acme/foo",
			query:   []any{"ref"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))

			response := serve(forwarding.handler, test.request)
			require.GreaterOrEqual(t, response.Code, 400, response.Body.String())

			lines := forwarding.auditLines(t)
			require.Len(t, lines, 1)

			assert.Equal(t, "refused", lines[0]["decision"])
			assert.Equal(t, test.reason, lines[0]["reason"])
			assert.Equal(t, test.host, lines[0]["host"])
			assert.Equal(t, test.method, lines[0]["method"])
			assert.Equal(t, test.path, lines[0]["path"])
			assert.Equal(t, test.query, lines[0]["query"])
		})
	}
}

func TestProxyLogsNoQueryValueOrTokenOfARefusedRequest(t *testing.T) {
	forwarding := newForwardingProxy(t)

	secrets := []string{
		"ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("b", 40),
		"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl",
	}

	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/applications/x/tokens/"+secrets[0]+"?access_token="+secrets[1]))
	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/x/"+secrets[2]+"/y?"+secrets[0]+"=1"))
	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/x/%67hp_"+strings.Repeat("c", 36)))

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 3)

	assert.Equal(t, "/applications/x/tokens/REDACTED", lines[0]["path"])
	assert.Equal(t, []any{"access_token"}, lines[0]["query"])
	assert.Equal(t, "/x/REDACTED/y", lines[1]["path"])
	assert.Equal(t, []any{"REDACTED"}, lines[1]["query"])
	assert.Equal(t, "/x/REDACTED", lines[2]["path"])

	for _, secret := range append(secrets, strings.Repeat("c", 36), validToken) {
		assert.NotContains(t, forwarding.audit.String(), secret)
	}
}

func TestProxyCutsTheLoggedPathOfARefusedRequest(t *testing.T) {
	forwarding := newForwardingProxy(t)

	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/"+strings.Repeat("a", 2000)))

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 1)

	path, ok := lines[0]["path"].(string)
	require.True(t, ok)
	assert.Len(t, path, 512)
}

func TestProxyLogsNoPathOfAnAllowedRequest(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo?ref=main"))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 1)

	assert.Equal(t, "allowed", lines[0]["decision"])
	assert.NotContains(t, lines[0], "path")
	assert.NotContains(t, lines[0], "query")
}
