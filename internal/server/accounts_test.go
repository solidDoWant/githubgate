package server_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// accountIDFixture is newFixture with one proxy rule for ci-build that may
// list the owner's repositories, and the proxy in front of the fake GitHub.
func accountIDFixture(t *testing.T) (*fixture, http.Handler) {
	t.Helper()

	f := newFixture(t)
	f.inputs.Policies[0].Spec.Rules = []githubgatev1alpha1.AccessRule{{
		Name:            "proxy",
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    []githubgatev1alpha1.Repository{"foo", "bar", "baz"},
		Permissions:     map[string]githubgatev1alpha1.Level{"issues": "read"},
		Proxy: &githubgatev1alpha1.ProxyMode{
			Enabled:         true,
			AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/list-for-org"},
		},
	}}
	f.rebuild()

	upstream, err := url.Parse(f.api.URL)
	require.NoError(t, err)

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules: func(namespace, serviceAccount string) []*model.Rule {
			return f.store.Load().Rules(namespace, serviceAccount)
		},
		Tokens:    f.server,
		Accounts:  f.server,
		Upstreams: proxy.Upstreams{API: upstream, Uploads: upstream, Git: upstream},
		Transport: http.DefaultTransport,
		Audit:     slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)

	return f, proxy.NewHandler(proxy.Options{Authenticator: headerAuthenticator{}, Forwarder: forwarder})
}

func TestProxyPaginatesThroughGitHubsOrganizationIDLinks(t *testing.T) {
	f, handler := accountIDFixture(t)

	items, visited, refused := paginate(t, handler, tenantNamespace+"/ci-build", "/orgs/"+owner+"/repos?per_page=1")
	require.Nil(t, refused, "a page was refused: %v", refused)

	names := []string{}
	for _, item := range items {
		names = append(names, fmt.Sprint(item["name"]))
	}

	assert.Equal(t, []string{"bar", "baz", "foo"}, names, "every repository is listed")
	require.Len(t, visited, 3)

	for _, next := range visited[1:] {
		assert.Contains(t, next, fmt.Sprintf("/organizations/%d/repos", f.fake.AccountID(owner)), "the fake GitHub's links name the organization by ID")
	}
}

func TestProxyRefusesAccountIDsNoInstallationOfTheCallerIsOn(t *testing.T) {
	f, handler := accountIDFixture(t)

	require.NoError(t, f.fake.CreateRepository("other", "private", false))

	tests := []struct {
		name   string
		caller string
		target string
	}{
		{name: "an organization the App has no installation on", caller: tenantNamespace + "/ci-build", target: fmt.Sprintf("/organizations/%d/repos?page=2", f.fake.AccountID("other"))},
		{name: "an account that doesn't exist", caller: tenantNamespace + "/ci-build", target: "/organizations/999999/repos?page=2"},
		{name: "a caller with no rules", caller: tenantNamespace + "/someone-else", target: fmt.Sprintf("/organizations/%d/repos?page=2", f.fake.AccountID(owner))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, refused := paginate(t, handler, test.caller, test.target)
			require.NotNil(t, refused, "the request was allowed")

			assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
			assert.Contains(t, refused.Body.String(), `"error":"denied"`)
			var body struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &body))
			assert.Contains(t, body.Message, "account ID")
			assert.NotContains(t, body.Message, "other", "the refusal names an account the caller asked for by ID")
			assert.NotContains(t, body.Message, owner, "the refusal names an account the caller asked for by ID")
		})
	}
}
