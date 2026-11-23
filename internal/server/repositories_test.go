package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// repositoryLookups counts the one-repository token requests the server
// sends the fake GitHub to name a repository ID, and answers them with
// GitHub's rate-limit response while rateLimited is set.
type repositoryLookups struct {
	count       atomic.Int64
	rateLimited atomic.Bool
}

func countRepositoryLookups(t *testing.T, f *fixture) *repositoryLookups {
	t.Helper()

	lookups := &repositoryLookups{}
	next := f.api.Config.Handler
	f.api.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/access_tokens") {
			next.ServeHTTP(writer, request)

			return
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		request.Body = io.NopCloser(bytes.NewReader(body))

		var mint struct {
			Permissions map[string]string `json:"permissions"`
		}
		if json.Unmarshal(body, &mint) == nil && len(mint.Permissions) == 1 && mint.Permissions["metadata"] == "read" {
			lookups.count.Add(1)

			if lookups.rateLimited.Load() {
				// GitHub's primary rate limit answer. The reset time is in the
				// past so the client doesn't hold back the next call itself.
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("X-RateLimit-Limit", "5000")
				writer.Header().Set("X-RateLimit-Remaining", "0")
				writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(`{"message":"API rate limit exceeded"}`))

				return
			}
		}

		next.ServeHTTP(writer, request)
	})

	return lookups
}

func TestProxyLooksUpAnUnreachedRepositoryIDOnceWhileCached(t *testing.T) {
	f, handler := repositoryIDFixture(t)
	lookups := countRepositoryLookups(t, f)

	for index := range 50 {
		_, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", fmt.Sprintf("/repositories/%d/issues", 900000+index%5))
		require.NotNil(t, refused, "the request was allowed")
		require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	}

	assert.Equal(t, int64(5), lookups.count.Load(), "each unreached ID is looked up once")

	f.clock.advance(time.Minute - time.Second)

	_, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", "/repositories/900000/issues")
	require.NotNil(t, refused, "the request was allowed")
	require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Equal(t, int64(5), lookups.count.Load(), "an unreached ID is kept for a minute")

	f.clock.advance(time.Second)

	_, _, refused = paginate(t, handler, tenantNamespace+"/ci-build", "/repositories/900000/issues")
	require.NotNil(t, refused, "the request was allowed")
	require.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())

	assert.Equal(t, int64(6), lookups.count.Load(), "an unreached ID is looked up again once its entry expires")
}

func TestProxyRefusesWithUnavailableWhileTheRepositoryLookupIsRateLimited(t *testing.T) {
	f, handler := repositoryIDFixture(t)
	lookups := countRepositoryLookups(t, f)

	id, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)

	lookups.rateLimited.Store(true)

	for range 3 {
		_, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", fmt.Sprintf("/repositories/%d/issues", id))
		require.NotNil(t, refused, "the request was allowed")
		assert.Equal(t, http.StatusServiceUnavailable, refused.Code, refused.Body.String())
	}

	assert.Equal(t, int64(3), lookups.count.Load(), "a rate-limited lookup is not cached")

	lookups.rateLimited.Store(false)

	_, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", fmt.Sprintf("/repositories/%d/issues/comments", id))
	require.Nil(t, refused, "the request was refused once the rate limit lifted: %v", refused)
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
