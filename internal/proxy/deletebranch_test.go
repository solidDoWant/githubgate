package proxy_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

// mintingTokens mints each proxy token from the fake GitHub, with the
// repositories and permissions of the decision.
type mintingTokens struct {
	client *github.AppClient
}

func (m *mintingTokens) ProxyToken(ctx context.Context, _ auth.Identity, decision proxy.Decision) (string, string, error) {
	token, err := m.client.Mint(ctx, github.MintRequest{
		Installation: decision.Installation.String(),
		Owner:        decision.Installation.Owner,
		Repositories: decision.Repositories,
		Permissions:  decision.Permissions,
	})
	if err != nil {
		return "", "github_refused", err
	}

	return token.Token, "minted", nil
}

// fakeGitHubThroughProxy serves the fake GitHub with acme/foo and puts the
// proxy, minting real tokens from the fake, in front of it as
// api.github.com. It returns the proxy's URL and the repository's git dir.
func fakeGitHubThroughProxy(t *testing.T, rules ...*model.Rule) (string, string) {
	t.Helper()

	root := t.TempDir()
	fake, err := fakegithub.New(root)
	require.NoError(t, err)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	fake.CreateApp(fakegithub.App{ID: 1, Name: "app", PublicKey: &key.PublicKey})
	require.NoError(t, fake.CreateInstallation(fakegithub.Installation{
		ID: 10, AppID: 1, Owner: "acme", Permissions: map[string]string{"contents": "write"},
	}))
	require.NoError(t, fake.CreateRepository("acme", "foo", false))

	upstream := httptest.NewServer(fake.Handler())
	t.Cleanup(upstream.Close)

	client, err := github.NewRegistry(github.Options{BaseURL: upstream.URL}).Client(
		types.NamespacedName{Namespace: "githubgate", Name: "app1"},
		github.Credentials{AppID: 1, PrivateKey: key, Version: "1"})
	require.NoError(t, err)

	base, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:     func(string, string) []*model.Rule { return rules },
		Tokens:    &mintingTokens{client: client},
		Upstreams: proxy.Upstreams{API: base, Uploads: base, Git: base},
	})
	require.NoError(t, err)

	handler := proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder})
	front := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		request.Host = proxy.HostAPI
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(front.Close)

	return front.URL, filepath.Join(root, "acme", "foo.git")
}

func TestGhPrMergeDeleteBranchThroughProxy(t *testing.T) {
	tests := []struct {
		name           string
		branch         string
		expectedStatus int
		expectedGone   bool
	}{
		{name: "a branch with slashes inside pushRefs is deleted", branch: "renovate/go-modules", expectedStatus: http.StatusNoContent, expectedGone: true},
		{name: "a branch with slashes outside pushRefs is kept", branch: "release/v1", expectedStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			front, gitDir := fakeGitHubThroughProxy(t,
				proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/renovate/**")))

			ref := "refs/heads/" + test.branch
			commit := strings.TrimSpace(runGit(t, gitDir, "-c", "user.name=x", "-c", "user.email=x@example.com",
				"commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "-m", "initial"))
			runGit(t, gitDir, "update-ref", ref, commit)

			// gh pr merge --delete-branch escapes every slash of the ref.
			target := front + "/repos/acme/foo/git/refs/" + url.PathEscape("heads/"+test.branch)
			require.Contains(t, target, "heads%2F")

			request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, target, nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "token "+validToken)
			request.Header.Set("Accept", "application/vnd.github+json")

			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())

			assert.Equal(t, test.expectedStatus, response.StatusCode)

			refs := strings.TrimSpace(runGit(t, gitDir, "for-each-ref", "--format=%(refname)"))
			if test.expectedGone {
				assert.Empty(t, refs)
			} else {
				assert.Equal(t, ref, refs)
			}
		})
	}
}
