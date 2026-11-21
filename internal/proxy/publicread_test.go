package proxy_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

func publicRead() ruleOption {
	return func(rule *model.Rule) { rule.Proxy.PublicRead = true }
}

func webPaths() ruleOption {
	return func(rule *model.Rule) { rule.Proxy.WebPaths = true }
}

// uploadPackRequest is a minimal git-upload-pack request body.
func uploadPackRequest(method, host, target string) *http.Request {
	request := authenticated(method, host, target)
	request.Body = http.NoBody

	if method == http.MethodPost {
		request = withBody(request, "0000")
		request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	}

	return request
}

func TestProxyForwardsPublicReadsWithAMetadataOnlyToken(t *testing.T) {
	tests := []struct {
		name    string
		request *http.Request
	}{
		{name: "git info/refs of another owner's repository", request: uploadPackRequest(http.MethodGet, proxy.HostGit, "/other/public/info/refs?service=git-upload-pack")},
		{name: "git-upload-pack of another owner's repository", request: uploadPackRequest(http.MethodPost, proxy.HostGit, "/other/public.git/git-upload-pack")},
		{name: "a repository of another owner", request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/other/public")},
		{name: "a commit of another owner's repository", request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/other/public/commits/main")},
		{name: "a tarball of another owner's repository", request: authenticated(http.MethodHead, proxy.HostAPI, "/repos/other/public/tarball/main")},
		{name: "a user", request: authenticated(http.MethodGet, proxy.HostAPI, "/users/octocat")},
		{name: "another organization's repositories", request: authenticated(http.MethodGet, proxy.HostAPI, "/orgs/other/repos")},
		{name: "another user's repositories", request: authenticated(http.MethodGet, proxy.HostAPI, "/users/other/repos")},
		{name: "the rate limit", request: authenticated(http.MethodGet, proxy.HostAPI, "/rate_limit")},
		{name: "a repository search", request: authenticated(http.MethodGet, proxy.HostAPI, "/search/repositories?q=nix")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t,
				proxyRule("own", app1Installation, []string{"foo", "app-*"}, write("contents"), publicRead()))

			response := serve(forwarding.handler, test.request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, forwarding.upstream.requests(), 1)
			require.Len(t, forwarding.tokens.decisions, 1)

			decision := forwarding.tokens.decisions[0]
			assert.Equal(t, proxy.AccessPublicRead, decision.Access)
			assert.Equal(t, map[string]permissions.Level{"metadata": permissions.LevelRead}, decision.Permissions)
			assert.Equal(t, []string{"foo"}, decision.Repositories)
			assert.Equal(t, app1Installation, decision.Installation)
		})
	}
}

func TestProxyPassesGitHubsNotFoundForAPrivateRepositoryOfAnotherOwner(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, write("contents"), publicRead()))
	forwarding.upstream.respond = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/other/private/contents/secret.txt"))

	require.Equal(t, http.StatusNotFound, response.Code)
	assert.JSONEq(t, `{"message":"Not Found"}`, response.Body.String())
	require.Len(t, forwarding.tokens.decisions, 1)
	assert.Equal(t, map[string]permissions.Level{"metadata": permissions.LevelRead}, forwarding.tokens.decisions[0].Permissions)
}

func TestProxyRefusesWritesOutsideTheInstallationWithPublicRead(t *testing.T) {
	tests := []struct {
		name    string
		request *http.Request
	}{
		{name: "POST", request: withBody(authenticated(http.MethodPost, proxy.HostAPI, "/repos/other/public/issues"), `{"title":"x"}`)},
		{name: "PATCH", request: withBody(authenticated(http.MethodPatch, proxy.HostAPI, "/repos/other/public"), `{"description":"x"}`)},
		{name: "PUT", request: withBody(authenticated(http.MethodPut, proxy.HostAPI, "/repos/other/public/subscription"), `{}`)},
		{name: "DELETE", request: authenticated(http.MethodDelete, proxy.HostAPI, "/repos/other/public")},
		{name: "git info/refs of receive-pack", request: authenticated(http.MethodGet, proxy.HostGit, "/other/public/info/refs?service=git-receive-pack")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, write("contents"), publicRead(), webPaths()))

			response := serve(forwarding.handler, test.request)

			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "other/public")
			assert.Empty(t, forwarding.upstream.requests())
			assert.Empty(t, forwarding.tokens.decisions)
		})
	}
}

func TestProxyKeepsTheRulesForTheInstallationsOwnRepositoriesWithPublicRead(t *testing.T) {
	tests := []struct {
		name    string
		request *http.Request
	}{
		{name: "REST read of an uncovered repository", request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/bar")},
		{name: "REST read of an uncovered repository in another case", request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/ACME/bar")},
		{name: "git fetch of an uncovered repository", request: uploadPackRequest(http.MethodGet, proxy.HostGit, "/acme/bar/info/refs?service=git-upload-pack")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, write("contents"), publicRead()))

			response := serve(forwarding.handler, test.request)

			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "/bar")
			assert.Empty(t, forwarding.upstream.requests())
		})
	}
}

func TestProxyKeepsTheRulesForTheInstallationOwnersAccountPathsWithPublicRead(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "the organization's repositories", target: "/orgs/acme/repos"},
		{name: "the user's repositories", target: "/users/acme/repos"},
		{name: "the organization's repositories in another case", target: "/orgs/ACME/repos"},
		{name: "the user's repositories in another case", target: "/users/Acme/repos"},
		{name: "the organization's custom property values", target: "/orgs/acme/properties/values"},
		{name: "the owner as a member of another organization", target: "/orgs/other/members/acme"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, read("contents"), publicRead()))

			response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, test.target))

			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Empty(t, forwarding.tokens.decisions)
			assert.Empty(t, forwarding.upstream.requests())
		})
	}
}

func TestProxyForwardsWebPathsWithNoCredential(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, read("contents"), webPaths()))
	forwarding.upstream.respond = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://objects.githubusercontent.com/asset?sig=short-lived")
		writer.WriteHeader(http.StatusFound)
	}

	request := authenticated(http.MethodGet, proxy.HostGit, "/other/public/releases/latest/download/tool.tar.gz")
	request.Header.Set("Cookie", "user_session=1")

	response := serve(forwarding.handler, request)

	require.Equal(t, http.StatusFound, response.Code, response.Body.String())
	assert.Equal(t, "https://objects.githubusercontent.com/asset?sig=short-lived", response.Header().Get("Location"))

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, "/git/other/public/releases/latest/download/tool.tar.gz", received[0].path)
	assert.Empty(t, received[0].header.Values("Authorization"))
	assert.Empty(t, received[0].header.Values("Cookie"))
	assert.Empty(t, forwarding.tokens.decisions, "a token was asked for")
}

func TestProxyPassesAWebPathsUnauthorizedAnswerThrough(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, read("contents"), webPaths()))
	forwarding.upstream.respond = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}

	response := serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostGit, "/settings"))

	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestProxyRefusesPublicReadsAndWebPathsWithoutTheSettings(t *testing.T) {
	tests := []struct {
		name           string
		request        *http.Request
		expectedStatus int
	}{
		{name: "git fetch of another owner", request: uploadPackRequest(http.MethodGet, proxy.HostGit, "/other/public/info/refs?service=git-upload-pack"), expectedStatus: http.StatusForbidden},
		{name: "REST read of another owner", request: authenticated(http.MethodGet, proxy.HostAPI, "/repos/other/public"), expectedStatus: http.StatusForbidden},
		{name: "a user", request: authenticated(http.MethodGet, proxy.HostAPI, "/users/octocat"), expectedStatus: http.StatusForbidden},
		{name: "a release download", request: authenticated(http.MethodGet, proxy.HostGit, "/other/public/releases/latest/download/tool.tar.gz"), expectedStatus: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, write("contents")))

			response := serve(forwarding.handler, test.request)

			require.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.Empty(t, forwarding.upstream.requests())
			assert.Empty(t, forwarding.tokens.decisions)
		})
	}
}

func TestProxyAuditsPublicReadsAndWebPaths(t *testing.T) {
	forwarding := newForwardingProxy(t, proxyRule("own", app1Installation, []string{"foo"}, read("contents"), publicRead(), webPaths()))

	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/other/public"))
	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostGit, "/other/public/archive/refs/heads/main.tar.gz"))
	serve(forwarding.handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/acme/foo"))
	serve(forwarding.handler, authenticated(http.MethodDelete, proxy.HostAPI, "/repos/other/public"))

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 4)

	assert.Equal(t, "public_read", lines[0]["access"])
	assert.Equal(t, "rest", lines[0]["kind"])
	assert.Equal(t, []any{"other/public"}, lines[0]["repositories"])
	assert.Equal(t, "metadata:read", lines[0]["permissions"])
	assert.Equal(t, "allowed", lines[0]["decision"])

	assert.Equal(t, "web", lines[1]["access"])
	assert.Equal(t, "web", lines[1]["kind"])
	assert.Equal(t, "", lines[1]["installation"])
	assert.Equal(t, "allowed", lines[1]["decision"])

	assert.Equal(t, "rules", lines[2]["access"])
	assert.Equal(t, "allowed", lines[2]["decision"])

	assert.Equal(t, "", lines[3]["access"])
	assert.Equal(t, "refused", lines[3]["decision"])
	assert.Equal(t, []any{"other/public"}, lines[3]["repositories"])

	assert.False(t, bytes.Contains(forwarding.audit.Bytes(), []byte(installationToken)))
}
