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

// fakeRepositoryResolver names repositories by ID and records the
// installations it was asked through.
type fakeRepositoryResolver struct {
	mu            sync.Mutex
	names         map[int64][2]string
	failure       error
	installations [][]*model.Installation
}

func (f *fakeRepositoryResolver) RepositoryName(_ context.Context, installations []*model.Installation, id int64) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.installations = append(f.installations, installations)

	if f.failure != nil {
		return "", "", f.failure
	}

	name, ok := f.names[id]
	if !ok {
		return "", "", proxy.ErrRepositoryNotFound
	}

	return name[0], name[1], nil
}

func acmeRepositories() *fakeRepositoryResolver {
	return &fakeRepositoryResolver{names: map[int64][2]string{42: {"acme", "foo"}, 43: {"acme", "bar"}}}
}

func TestProxyTreatsARepositoryIDPathAsTheNamedRepositoryPath(t *testing.T) {
	resolver := acmeRepositories()
	forwarding := newForwardingProxyWith(t, resolver, proxyRule("a", app1Installation, []string{"foo"}, read("issues")))

	byName := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo/issues/comments?page=2&per_page=100"))
	require.Equal(t, http.StatusOK, byName.Code, byName.Body.String())

	byID := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repositories/42/issues/comments?page=2&per_page=100"))
	require.Equal(t, http.StatusOK, byID.Code, byID.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 2)
	assert.Equal(t, received[0].path, received[1].path, "the ID form is forwarded as the named form")
	assert.Equal(t, "/api/repos/acme/foo/issues/comments", received[1].path)
	assert.Equal(t, received[0].query, received[1].query)
	assert.Equal(t, []string{"token " + installationToken}, received[1].header.Values("Authorization"))

	require.Len(t, forwarding.tokens.decisions, 2)
	assert.Equal(t, forwarding.tokens.decisions[0], forwarding.tokens.decisions[1], "the ID form gets another token")

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 2)
	assert.Equal(t, lines[0]["operation"], lines[1]["operation"])
	assert.Equal(t, "issues/list-comments-for-repo", lines[1]["operation"])

	require.Len(t, resolver.installations, 1)
	assert.Equal(t, []*model.Installation{app1Installation}, resolver.installations[0], "the ID is looked up through the caller's installations")
}

func TestProxyRefusesRepositoryIDPathsTheRulesDoNotCover(t *testing.T) {
	coversFoo := []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("issues"))}

	tests := []struct {
		name            string
		resolver        *fakeRepositoryResolver
		rules           []*model.Rule
		target          string
		expectedStatus  int
		expectedCode    string
		expectedMessage string
		hiddenName      string
	}{
		{
			name:            "a repository no rule covers",
			resolver:        acmeRepositories(),
			rules:           coversFoo,
			target:          "/repositories/43/issues/comments?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "repository ID 43: it is not covered by any of your proxy rules",
			hiddenName:      "bar",
		},
		{
			name:            "an ID no installation reaches",
			resolver:        acmeRepositories(),
			rules:           coversFoo,
			target:          "/repositories/44/issues/comments?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "repository ID 44: it is not covered by any of your proxy rules",
		},
		{
			name:            "a covered repository with a refused operation",
			resolver:        acmeRepositories(),
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("issues"), denyOperations("issues/list-comments-for-repo"))},
			target:          "/repositories/42/issues/comments?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "issues/list-comments-for-repo",
		},
		{
			name:            "a caller with no rules",
			resolver:        acmeRepositories(),
			target:          "/repositories/42/issues/comments?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "repository ID 42: it is not covered by any of your proxy rules",
			hiddenName:      "foo",
		},
		{
			name:            "no resolver",
			rules:           coversFoo,
			target:          "/repositories/42/issues/comments?page=2",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "repository ID 42: it is not covered by any of your proxy rules",
		},
		{
			name:            "a lookup that fails",
			resolver:        &fakeRepositoryResolver{failure: errors.New("GitHub is down")},
			rules:           coversFoo,
			target:          "/repositories/42/issues/comments?page=2",
			expectedStatus:  http.StatusServiceUnavailable,
			expectedCode:    "github_unavailable",
			expectedMessage: "repository ID 42 can't be looked up now",
		},
		{
			name:            "an ID path of no repository operation",
			resolver:        acmeRepositories(),
			rules:           coversFoo,
			target:          "/repositories/42/no-such-thing",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
		{
			name:            "an ID with a leading zero",
			resolver:        acmeRepositories(),
			rules:           coversFoo,
			target:          "/repositories/042/issues/comments",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
		{
			name:            "an ID too large for GitHub",
			resolver:        acmeRepositories(),
			rules:           coversFoo,
			target:          "/repositories/99999999999999999999/issues/comments",
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "unknown_operation",
			expectedMessage: "matches no operation",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var resolver proxy.RepositoryResolver
			if test.resolver != nil {
				resolver = test.resolver
			}

			forwarding := newForwardingProxyWith(t, resolver, test.rules...)

			response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, test.target))

			require.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"error":"`+test.expectedCode+`"`)
			assert.Contains(t, response.Body.String(), test.expectedMessage)

			if test.hiddenName != "" {
				assert.NotContains(t, response.Body.String(), test.hiddenName, "the refusal names a repository the caller asked for by ID")
			}

			assert.Empty(t, forwarding.upstream.requests(), "a refused request was forwarded")
			assert.Empty(t, forwarding.tokens.decisions, "a token was asked for")
		})
	}
}
