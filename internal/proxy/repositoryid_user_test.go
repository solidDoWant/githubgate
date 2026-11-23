package proxy_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// A user access token reaches every repository its user can, so GitHub
// can't refuse a request sent to a repository that took a renamed
// repository's old name. A user rule's request by repository ID is checked
// with GitHub before it is forwarded.
func TestProxyChecksTheRepositoryIDOfUserRuleRequestsWithGitHub(t *testing.T) {
	const listComments = "issues/list-comments-for-repo"

	tests := []struct {
		name            string
		current         map[int64][2]string
		currentFailure  error
		expectedStatus  int
		expectedCode    string
		expectedForward bool
	}{
		{
			name:            "the ID still names the repository",
			expectedStatus:  http.StatusOK,
			expectedForward: true,
		},
		{
			name:           "the repository was renamed",
			current:        map[int64][2]string{42: {"acme", "foo-old"}},
			expectedStatus: http.StatusForbidden,
			expectedCode:   "denied",
		},
		{
			name:           "no installation reaches the ID any more",
			current:        map[int64][2]string{},
			expectedStatus: http.StatusForbidden,
			expectedCode:   "denied",
		},
		{
			name:           "GitHub can't be reached",
			currentFailure: errors.New("connection refused"),
			expectedStatus: http.StatusServiceUnavailable,
			expectedCode:   "unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := acmeRepositories()
			resolver.current, resolver.currentFailure = test.current, test.currentFailure

			upstream := &fakeUpstream{}
			server := httptest.NewTLSServer(upstream)
			t.Cleanup(server.Close)

			base, err := url.Parse(server.URL)
			require.NoError(t, err)

			rules := []*model.Rule{
				proxyRule("installation", app1Installation, []string{"foo"}, read("issues")),
				userRule("comments", ownerAuthorization, listComments),
			}
			tokens := &fakeTokens{}

			forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
				Rules:        func(string, string) []*model.Rule { return rules },
				Tokens:       tokens,
				UserTokens:   &fakeUserTokens{tokens: map[types.NamespacedName]string{ownerAuthorization.NamespacedName: "user-access-token"}},
				Repositories: resolver,
				Upstreams:    proxy.Upstreams{API: base, Uploads: base, Git: base},
				Transport:    server.Client().Transport,
				Audit:        slog.New(slog.DiscardHandler),
			})
			require.NoError(t, err)

			handler := proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder})

			response := serve(handler, authenticated(http.MethodGet, proxy.HostAPI, "/repositories/42/issues/comments"))
			require.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.Equal(t, 1, resolver.currentAsked, "the ID is checked with GitHub")
			assert.Empty(t, tokens.decisions, "no installation token is asked for")

			received := upstream.requests()
			if !test.expectedForward {
				assert.Empty(t, received, "nothing is forwarded")
				assert.Contains(t, response.Body.String(), test.expectedCode)
				assert.Contains(t, response.Body.String(), "repository ID 42")
				assert.NotContains(t, response.Body.String(), "foo")

				return
			}

			require.Len(t, received, 1)
			assert.Equal(t, "/repos/acme/foo/issues/comments", received[0].path)
			assert.Equal(t, []string{"token user-access-token"}, received[0].header.Values("Authorization"))
		})
	}
}
