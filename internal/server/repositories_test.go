package server_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// repositoryIDFixture is newFixture with one proxy rule for ci-build that
// reads the issues of foo, and the proxy in front of the fake GitHub.
func repositoryIDFixture(t *testing.T) (*fixture, http.Handler) {
	t.Helper()

	f := newFixture(t)
	f.inputs.Policies[0].Spec.Rules = []githubgatev1alpha1.AccessRule{{
		Name:            "proxy",
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    []githubgatev1alpha1.Repository{"foo"},
		Permissions:     map[string]githubgatev1alpha1.Level{"issues": "read"},
		Proxy:           &githubgatev1alpha1.ProxyMode{Enabled: true},
	}}
	f.rebuild()

	upstream, err := url.Parse(f.api.URL)
	require.NoError(t, err)

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules: func(namespace, serviceAccount string) []*model.Rule {
			return f.store.Load().Rules(namespace, serviceAccount)
		},
		Tokens:       f.server,
		Repositories: f.server,
		Upstreams:    proxy.Upstreams{API: upstream, Uploads: upstream, Git: upstream},
		Transport:    http.DefaultTransport,
		Audit:        slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)

	return f, proxy.NewHandler(proxy.Options{Authenticator: headerAuthenticator{}, Forwarder: forwarder})
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// paginate gets target through the proxy and follows each next link, as
// gh api --paginate and client libraries do, sending each link's path and
// query to api.github.com, which the proxy stands in for.
func paginate(t *testing.T, handler http.Handler, caller, target string) ([]map[string]any, []string, *httptest.ResponseRecorder) {
	t.Helper()

	var (
		items   []map[string]any
		visited []string
	)

	for target != "" {
		visited = append(visited, target)

		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		request.Host = proxy.HostAPI
		request.Header.Set("Authorization", "Bearer "+caller)

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			return items, visited, response
		}

		var page []map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))

		items = append(items, page...)
		target = ""

		if match := nextLink.FindStringSubmatch(response.Header().Get("Link")); match != nil {
			next, err := url.Parse(match[1])
			require.NoError(t, err)

			target = next.RequestURI()
		}
	}

	return items, visited, nil
}

func TestProxyPaginatesThroughGitHubsRepositoryIDLinks(t *testing.T) {
	f, handler := repositoryIDFixture(t)

	for index := range 7 {
		f.fake.AddIssueComment(owner, "foo", fmt.Sprintf("comment %d", index))
	}

	id, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)

	mintsBefore := len(f.fake.MintLog())

	items, visited, refused := paginate(t, handler, tenantNamespace+"/ci-build", "/repos/"+owner+"/foo/issues/comments?per_page=2")
	require.Nil(t, refused, "a page was refused: %v", refused)

	require.Len(t, items, 7, "every comment is listed")
	require.Len(t, visited, 4)

	for _, next := range visited[1:] {
		assert.Contains(t, next, fmt.Sprintf("/repositories/%d/issues/comments", id), "the fake GitHub's links name the repository by ID")
	}

	mints := f.fake.MintLog()[mintsBefore:]
	lookups := 0

	for _, mint := range mints {
		if len(mint.Permissions) == 1 && mint.Permissions["metadata"] == "read" {
			lookups++
		}
	}

	assert.Equal(t, 1, lookups, "the repository ID is looked up once, not once per page: %+v", mints)
}

func TestProxyRefusesRepositoryIDsOutsideTheCallersRules(t *testing.T) {
	f, handler := repositoryIDFixture(t)

	require.NoError(t, f.fake.CreateRepository("other", "private", false))

	barID, ok := f.fake.RepositoryID(owner, "bar")
	require.True(t, ok)

	otherID, ok := f.fake.RepositoryID("other", "private")
	require.True(t, ok)

	tests := []struct {
		name   string
		caller string
		target string
	}{
		{name: "a repository of the installation no rule covers", caller: tenantNamespace + "/ci-build", target: fmt.Sprintf("/repositories/%d/issues/comments?page=2", barID)},
		{name: "a repository the installation doesn't reach", caller: tenantNamespace + "/ci-build", target: fmt.Sprintf("/repositories/%d/issues/comments?page=2", otherID)},
		{name: "a repository that doesn't exist", caller: tenantNamespace + "/ci-build", target: "/repositories/999999/issues/comments?page=2"},
		{name: "a caller with no rules", caller: tenantNamespace + "/someone-else", target: "/repositories/999999/issues/comments?page=2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, refused := paginate(t, handler, test.caller, test.target)
			require.NotNil(t, refused, "the request was allowed")

			assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
			assert.Contains(t, refused.Body.String(), `"error":"denied"`)
			assert.NotContains(t, refused.Body.String(), `bar`)
			assert.NotContains(t, refused.Body.String(), `private`)
		})
	}
}

func TestProxyAnswersAnUncoveredRepositoryIDTheSameWhetherOrNotItIsReached(t *testing.T) {
	f, handler := repositoryIDFixture(t)

	require.NoError(t, f.fake.CreateRepository("other", "private", false))

	barID, ok := f.fake.RepositoryID(owner, "bar")
	require.True(t, ok)

	otherID, ok := f.fake.RepositoryID("other", "private")
	require.True(t, ok)

	bodies := []struct {
		name string
		body string
	}{
		{name: "a body that is not JSON", body: "not json"},
		{name: "a body larger than the limit", body: `{"ref":"refs/heads/x","sha":"` + strings.Repeat("a", 2<<20) + `"}`},
		{name: "a valid body", body: `{"ref":"refs/heads/x","sha":"aaaa"}`},
	}

	ids := map[string]int64{
		"reached by the installation, no rule covers it": barID,
		"not reached by the installation":                otherID,
		"not existing":                                   999999,
	}

	for _, body := range bodies {
		t.Run(body.name, func(t *testing.T) {
			answers := map[string]string{}

			for name, id := range ids {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/repositories/%d/git/refs", id), strings.NewReader(body.body))
				request.Host = proxy.HostAPI
				request.Header.Set("Authorization", "Bearer "+tenantNamespace+"/ci-build")

				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)

				assert.Equal(t, http.StatusForbidden, response.Code, "%s: %s", name, response.Body.String())

				answers[name] = fmt.Sprintf("%d %s", response.Code, strings.ReplaceAll(response.Body.String(), strconv.FormatInt(id, 10), "N"))
			}

			assert.Equal(t, answers["not existing"], answers["reached by the installation, no rule covers it"])
			assert.Equal(t, answers["not existing"], answers["not reached by the installation"])
			assert.Contains(t, answers["not existing"], `"error":"denied"`)
		})
	}
}
