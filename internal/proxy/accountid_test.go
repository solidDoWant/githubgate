package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// fakeAccountResolver names accounts by ID and records the installations
// it was asked through.
type fakeAccountResolver struct {
	mu            sync.Mutex
	logins        map[int64]string
	failure       error
	installations [][]*model.Installation
}

func (f *fakeAccountResolver) AccountLogin(_ context.Context, installations []*model.Installation, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.installations = append(f.installations, installations)

	if f.failure != nil {
		return "", f.failure
	}

	login, ok := f.logins[id]
	if !ok {
		return "", proxy.ErrAccountNotFound
	}

	return login, nil
}

func (f *fakeAccountResolver) asked() [][]*model.Installation {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]*model.Installation(nil), f.installations...)
}

func acmeAccount() *fakeAccountResolver {
	return &fakeAccountResolver{logins: map[int64]string{7: "acme"}}
}

// listsAcmeRepositories may list the repositories of the organization or
// user acme.
func listsAcmeRepositories() *model.Rule {
	return proxyRule("a", app1Installation, []string{"foo"}, read("metadata"), allowOperations("repos/list-for-org", "repos/list-for-user"))
}

func TestProxyTreatsAnAccountIDPathAsTheNamedAccountPath(t *testing.T) {
	tests := []struct {
		name              string
		byName            string
		byID              string
		expectedPath      string
		expectedOperation string
	}{
		{
			name:              "an organization's repositories",
			byName:            "/orgs/acme/repos?page=2&per_page=100",
			byID:              "/organizations/7/repos?page=2&per_page=100",
			expectedPath:      "/api/orgs/acme/repos",
			expectedOperation: "repos/list-for-org",
		},
		{
			name:              "a user's repositories",
			byName:            "/users/acme/repos?page=2&per_page=100",
			byID:              "/user/7/repos?page=2&per_page=100",
			expectedPath:      "/api/users/acme/repos",
			expectedOperation: "repos/list-for-user",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := acmeAccount()
			forwarding := newForwardingProxyWithResolvers(t, nil, resolver, listsAcmeRepositories())

			byName := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, test.byName))
			require.Equal(t, http.StatusOK, byName.Code, byName.Body.String())

			byID := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, test.byID))
			require.Equal(t, http.StatusOK, byID.Code, byID.Body.String())

			received := forwarding.upstream.requests()
			require.Len(t, received, 2)
			assert.Equal(t, test.expectedPath, received[0].path)
			assert.Equal(t, test.expectedPath, received[1].path, "the ID form is forwarded as the named form")
			assert.Equal(t, received[0].query, received[1].query)
			assert.Equal(t, []string{"token " + installationToken}, received[1].header.Values("Authorization"))

			require.Len(t, forwarding.tokens.decisions, 2)
			assert.Equal(t, forwarding.tokens.decisions[0], forwarding.tokens.decisions[1], "the ID form gets another token")

			lines := forwarding.auditLines(t)
			require.Len(t, lines, 2)
			assert.Equal(t, test.expectedOperation, lines[0]["operation"])
			assert.Equal(t, lines[0]["operation"], lines[1]["operation"])

			asked := resolver.asked()
			require.Len(t, asked, 1)
			assert.Equal(t, []*model.Installation{app1Installation}, asked[0], "the ID is looked up through the caller's installations")
		})
	}
}

func TestProxyDecidesAnAccountIDPathAsTheNamedAccountPath(t *testing.T) {
	tests := []struct {
		name  string
		rules []*model.Rule
	}{
		{
			name:  "an operation the rules don't allow",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("metadata"))},
		},
		{
			name: "an operation the rules deny",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("metadata"),
				allowOperations("repos/list-for-org"), denyOperations("repos/list-for-org"))},
		},
		{
			// Public read never applies to a request that names an
			// installation's own owner, by login or by ID.
			name: "a public read of the installation's own owner",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("metadata"),
				func(rule *model.Rule) { rule.Proxy.PublicRead = true })},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxyWithResolvers(t, nil, acmeAccount(), test.rules...)

			byName := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/orgs/acme/repos?page=2"))
			require.Equal(t, http.StatusForbidden, byName.Code, byName.Body.String())

			byID := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/organizations/7/repos?page=2"))
			assert.Equal(t, byName.Code, byID.Code)
			assert.Equal(t, byName.Body.String(), byID.Body.String(), "the ID form is decided as the named form")

			assert.Empty(t, forwarding.upstream.requests(), "a refused request was forwarded")
			assert.Empty(t, forwarding.tokens.decisions, "a token was asked for")
		})
	}
}

func TestProxyRefusesAccountIDPathsNoInstallationIsOn(t *testing.T) {
	tests := []struct {
		name            string
		resolver        *fakeAccountResolver
		rules           []*model.Rule
		target          string
		expectedStatus  int
		expectedCode    string
		expectedMessage string
	}{
		{
			name:            "an organization ID no installation is on",
			resolver:        acmeAccount(),
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/organizations/8/repos?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "account ID 8: it is not covered by any of your proxy rules",
		},
		{
			name:            "a user ID no installation is on",
			resolver:        acmeAccount(),
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/user/8/repos?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "account ID 8: it is not covered by any of your proxy rules",
		},
		{
			name:            "a caller with no rules",
			resolver:        acmeAccount(),
			target:          "/organizations/7/repos?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "account ID 7: it is not covered by any of your proxy rules",
		},
		{
			name:            "no resolver",
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/organizations/7/repos?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "account ID 7: it is not covered by any of your proxy rules",
		},
		{
			name:            "a lookup that fails",
			resolver:        &fakeAccountResolver{failure: errors.New("GitHub is down")},
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/organizations/7/repos?page=2",
			expectedStatus:  http.StatusServiceUnavailable,
			expectedCode:    "github_unavailable",
			expectedMessage: "account ID 7 can't be looked up now",
		},
		{
			name:            "an ID path of no account operation",
			resolver:        acmeAccount(),
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/organizations/7/no-such-thing",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
		{
			name:            "an ID with a leading zero",
			resolver:        acmeAccount(),
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/organizations/07/repos",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
		{
			name:            "an ID too large for GitHub",
			resolver:        acmeAccount(),
			rules:           []*model.Rule{listsAcmeRepositories()},
			target:          "/user/99999999999999999999/repos",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var resolver proxy.AccountResolver
			if test.resolver != nil {
				resolver = test.resolver
			}

			forwarding := newForwardingProxyWithResolvers(t, nil, resolver, test.rules...)

			response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, test.target))

			require.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"error":"`+test.expectedCode+`"`)
			assert.Contains(t, response.Body.String(), test.expectedMessage)
			assert.NotContains(t, response.Body.String(), "acme", "the refusal names an account the caller asked for by ID")

			assert.Empty(t, forwarding.upstream.requests(), "a refused request was forwarded")
			assert.Empty(t, forwarding.tokens.decisions, "a token was asked for")
		})
	}
}

func TestProxyKeepsAccountPathsThatMatchAnOperationAsSent(t *testing.T) {
	resolver := acmeAccount()
	forwarding := newForwardingProxyWithResolvers(t, nil, resolver,
		proxyRule("a", app1Installation, []string{"foo"}, read("metadata"), allowOperations("users/get-by-id")))

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/user/7"))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, "/api/user/7", received[0].path)

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 1)
	assert.Equal(t, "users/get-by-id", lines[0]["operation"])

	assert.Empty(t, resolver.asked(), "a path that matches an operation as sent was looked up as an account ID")
}
