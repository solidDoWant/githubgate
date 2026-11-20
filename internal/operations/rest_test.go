package operations

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchREST(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		id       string
		template string
		params   map[string]string
	}{
		{
			name: "repository operation", method: http.MethodPost, path: "/repos/acme/foo/releases",
			id: "repos/create-release", template: "/repos/{owner}/{repo}/releases",
			params: map[string]string{"owner": "acme", "repo": "foo"},
		},
		{
			name: "same path, other method", method: http.MethodGet, path: "/repos/acme/foo/releases",
			id: "repos/list-releases", template: "/repos/{owner}/{repo}/releases",
			params: map[string]string{"owner": "acme", "repo": "foo"},
		},
		{
			name: "operation without a repository", method: http.MethodGet, path: "/user",
			id: "users/get-authenticated", template: "/user", params: map[string]string{},
		},
		{
			name: "root", method: http.MethodGet, path: "/",
			id: "meta/root", template: "/", params: map[string]string{},
		},
		{
			name: "organization operation", method: http.MethodGet, path: "/orgs/acme/repos",
			id: "repos/list-for-org", template: "/orgs/{org}/repos", params: map[string]string{"org": "acme"},
		},
		{
			name: "upload to uploads.github.com", method: http.MethodPost, path: "/repos/acme/foo/releases/42/assets",
			id: "repos/upload-release-asset", template: "/repos/{owner}/{repo}/releases/{release_id}/assets",
			params: map[string]string{"owner": "acme", "repo": "foo", "release_id": "42"},
		},
		{
			name: "list release assets", method: http.MethodGet, path: "/repos/acme/foo/releases/42/assets",
			id: "repos/list-release-assets", template: "/repos/{owner}/{repo}/releases/{release_id}/assets",
			params: map[string]string{"owner": "acme", "repo": "foo", "release_id": "42"},
		},
		{
			name: "ref with slashes", method: http.MethodPatch, path: "/repos/acme/foo/git/refs/heads/agent/x",
			id: "git/update-ref", template: "/repos/{owner}/{repo}/git/refs/{ref}",
			params: map[string]string{"owner": "acme", "repo": "foo", "ref": "heads/agent/x"},
		},
		{
			name: "ref of one segment", method: http.MethodDelete, path: "/repos/acme/foo/git/refs/x",
			id: "git/delete-ref", template: "/repos/{owner}/{repo}/git/refs/{ref}",
			params: map[string]string{"owner": "acme", "repo": "foo", "ref": "x"},
		},
		{
			name: "create ref without a ref in the path", method: http.MethodPost, path: "/repos/acme/foo/git/refs",
			id: "git/create-ref", template: "/repos/{owner}/{repo}/git/refs",
			params: map[string]string{"owner": "acme", "repo": "foo"},
		},
		{
			name: "file path with slashes", method: http.MethodPut, path: "/repos/acme/foo/contents/dir/sub/file.txt",
			id: "repos/create-or-update-file-contents", template: "/repos/{owner}/{repo}/contents/{path}",
			params: map[string]string{"owner": "acme", "repo": "foo", "path": "dir/sub/file.txt"},
		},
		{
			name: "file path read", method: http.MethodGet, path: "/repos/acme/foo/contents/dir/file",
			id: "repos/get-content", template: "/repos/{owner}/{repo}/contents/{path}",
			params: map[string]string{"owner": "acme", "repo": "foo", "path": "dir/file"},
		},
		{
			name: "archive of a ref with slashes", method: http.MethodGet, path: "/repos/acme/foo/tarball/heads/x",
			id: "repos/download-tarball-archive", template: "/repos/{owner}/{repo}/tarball/{ref}",
			params: map[string]string{"owner": "acme", "repo": "foo", "ref": "heads/x"},
		},
		{
			name: "compare base...head", method: http.MethodGet, path: "/repos/acme/foo/compare/a...b",
			id: "repos/compare-commits", template: "/repos/{owner}/{repo}/compare/{basehead}",
			params: map[string]string{"owner": "acme", "repo": "foo", "basehead": "a...b"},
		},
		{
			name: "compare branches with slashes", method: http.MethodGet, path: "/repos/acme/foo/compare/main...agent/x",
			id: "repos/compare-commits", template: "/repos/{owner}/{repo}/compare/{basehead}",
			params: map[string]string{"owner": "acme", "repo": "foo", "basehead": "main...agent/x"},
		},
		{
			name: "slash parameter followed by literals", method: http.MethodGet, path: "/repos/acme/foo/commits/agent/x/status",
			id: "repos/get-combined-status-for-ref", template: "/repos/{owner}/{repo}/commits/{ref}/status",
			params: map[string]string{"owner": "acme", "repo": "foo", "ref": "agent/x"},
		},
		{
			name: "literal beats a slash parameter", method: http.MethodGet, path: "/repos/acme/foo/commits/abc/comments",
			id: "repos/list-comments-for-commit", template: "/repos/{owner}/{repo}/commits/{commit_sha}/comments",
			params: map[string]string{"owner": "acme", "repo": "foo", "commit_sha": "abc"},
		},
		{
			name: "branch with slashes", method: http.MethodGet, path: "/repos/acme/foo/branches/agent/x",
			id: "repos/get-branch", template: "/repos/{owner}/{repo}/branches/{branch}",
			params: map[string]string{"owner": "acme", "repo": "foo", "branch": "agent/x"},
		},
		{
			name: "most literal template wins", method: http.MethodGet, path: "/repos/acme/foo/branches/agent/x/protection",
			id: "repos/get-branch-protection", template: "/repos/{owner}/{repo}/branches/{branch}/protection",
			params: map[string]string{"owner": "acme", "repo": "foo", "branch": "agent/x"},
		},
		{
			name: "literal segment before parameter", method: http.MethodGet, path: "/repos/acme/foo/releases/latest",
			id: "repos/get-latest-release", template: "/repos/{owner}/{repo}/releases/latest",
			params: map[string]string{"owner": "acme", "repo": "foo"},
		},
		{
			name: "parameter where no literal matches", method: http.MethodGet, path: "/repos/acme/foo/releases/7",
			id: "repos/get-release", template: "/repos/{owner}/{repo}/releases/{release_id}",
			params: map[string]string{"owner": "acme", "repo": "foo", "release_id": "7"},
		},
		{
			name: "release by tag with slashes", method: http.MethodGet, path: "/repos/acme/foo/releases/tags/v1/rc",
			id: "repos/get-release-by-tag", template: "/repos/{owner}/{repo}/releases/tags/{tag}",
			params: map[string]string{"owner": "acme", "repo": "foo", "tag": "v1/rc"},
		},
		{
			name: "HEAD is the GET of the path", method: http.MethodHead, path: "/repos/acme/foo/contents/a/b",
			id: "repos/get-content", template: "/repos/{owner}/{repo}/contents/{path}",
			params: map[string]string{"owner": "acme", "repo": "foo", "path": "a/b"},
		},
		{
			name: "HEAD of a repository", method: http.MethodHead, path: "/repos/acme/foo",
			id: "repos/get", template: "/repos/{owner}/{repo}",
			params: map[string]string{"owner": "acme", "repo": "foo"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match, ok := MatchREST(test.method, test.path)
			require.True(t, ok)
			assert.Equal(t, test.id, match.ID)
			assert.Equal(t, test.template, match.Template)
			assert.Equal(t, test.params, match.Params)
		})
	}
}

func TestMatchRESTOwnerAndRepo(t *testing.T) {
	match, ok := MatchREST(http.MethodGet, "/repos/acme/foo/pulls")
	require.True(t, ok)

	owner, named := match.Owner()
	assert.True(t, named)
	assert.Equal(t, "acme", owner)

	repo, named := match.Repo()
	assert.True(t, named)
	assert.Equal(t, "foo", repo)

	match, ok = MatchREST(http.MethodGet, "/orgs/acme")
	require.True(t, ok)

	_, named = match.Owner()
	assert.False(t, named)

	_, named = match.Repo()
	assert.False(t, named)

	match, ok = MatchREST(http.MethodGet, "/users/acme/repos")
	require.True(t, ok)

	_, named = match.Repo()
	assert.False(t, named)
}

func TestMatchRESTUnknown(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "unlisted path", method: http.MethodGet, path: "/repos/acme/foo/no-such-thing"},
		{name: "unlisted top level", method: http.MethodGet, path: "/nothing"},
		{name: "method the path doesn't have", method: http.MethodPut, path: "/repos/acme/foo/releases"},
		{name: "OPTIONS", method: http.MethodOptions, path: "/repos/acme/foo"},
		{name: "lower-case method", method: "get", path: "/repos/acme/foo"},
		{name: "slash parameter without a value", method: http.MethodGet, path: "/repos/acme/foo/contents"},
		{name: "extra segment after a single-segment parameter", method: http.MethodGet, path: "/repos/acme/foo/releases/7/x"},
		{name: "repository name with a slash", method: http.MethodGet, path: "/repos/acme/foo/bar"},
		{name: "empty segment", method: http.MethodGet, path: "/repos/acme//foo"},
		{name: "trailing slash", method: http.MethodGet, path: "/repos/acme/foo/"},
		{name: "empty segment in a slash parameter", method: http.MethodGet, path: "/repos/acme/foo/contents/a//b"},
		{name: "relative path", method: http.MethodGet, path: "repos/acme/foo"},
		{name: "empty path", method: http.MethodGet, path: ""},
		{name: "graphql endpoint as REST GET", method: http.MethodDelete, path: "/graphql"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ok := MatchREST(test.method, test.path)
			assert.False(t, ok)
		})
	}
}

// Every operation of the description is reached by a path built from its own
// template, unless a more literal template of the same method takes it.
func TestMatchRESTEveryCatalogueOperation(t *testing.T) {
	require.NotEmpty(t, restCatalogue)

	for _, op := range restCatalogue {
		parts := splitPath(op.Template)
		for index, part := range parts {
			if strings.HasPrefix(part, "{") {
				parts[index] = "v" + part[1:len(part)-1]
			}
		}

		path := "/" + strings.Join(parts, "/")

		match, ok := MatchREST(op.Method, path)
		require.True(t, ok, "%s %s", op.Method, path)

		if match.ID != op.ID {
			other := templateLiterals(match.Template)
			assert.Greater(t, other, templateLiterals(op.Template), "%s %s matched %s", op.Method, path, match.ID)
		}

		if op.Method == http.MethodGet {
			head, ok := MatchREST(http.MethodHead, path)
			require.True(t, ok)
			assert.Equal(t, match.ID, head.ID)
		}
	}
}

func templateLiterals(template string) int {
	count := 0

	for _, part := range splitPath(template) {
		if !strings.HasPrefix(part, "{") {
			count++
		}
	}

	return count
}
