package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy"
)

// Clients send other spellings of a canonical request. Each one here must be
// forwarded as the same request of the same repository.
func TestProxyForwardsTheRequestFormsClientsSend(t *testing.T) {
	tests := []struct {
		name     string
		request  func() *http.Request
		wantPath string
	}{
		{
			name:     "a HEAD request",
			request:  func() *http.Request { return authenticated(http.MethodHead, proxy.HostAPI, "/repos/acme/foo") },
			wantPath: "/api/repos/acme/foo",
		},
		{
			name:     "an owner and repository in another case",
			request:  func() *http.Request { return authenticated(http.MethodGet, proxy.HostAPI, "/repos/ACME/Foo") },
			wantPath: "/api/repos/ACME/Foo",
		},
		{
			name: "a lowercase encoded slash in a ref",
			request: func() *http.Request {
				return authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo/git/ref/heads%2fa%2fb")
			},
			wantPath: "/api/repos/acme/foo/git/ref/heads/a/b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))

			response := serve(forwarding.handler, test.request())
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			received := forwarding.upstream.requests()
			require.Len(t, received, 1)
			assert.Equal(t, test.wantPath, received[0].path)
		})
	}
}

func TestProxyPassesConditionalRequestsThrough(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents")))
	forwarding.upstream.respond = func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("If-None-Match") == `"etag-1"` {
			writer.WriteHeader(http.StatusNotModified)

			return
		}

		writer.WriteHeader(http.StatusOK)
	}

	request := authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo")
	request.Header.Set("If-None-Match", `"etag-1"`)

	response := serve(forwarding.handler, request)

	require.Equal(t, http.StatusNotModified, response.Code, response.Body.String())
	assert.Len(t, forwarding.upstream.requests(), 1)
}
