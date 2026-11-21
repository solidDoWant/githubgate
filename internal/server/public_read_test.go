package server_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

// publicReadFixture is newFixture with one public-read proxy rule for
// ci-build that names repositories exactly, and a public repository of
// another owner.
func publicReadFixture(t *testing.T, repositories ...githubgatev1alpha1.Repository) *fixture {
	t.Helper()

	f := newFixture(t)
	require.NoError(t, f.fake.CreateRepository("other", "public", true))

	f.inputs.Policies[0].Spec.Rules = []githubgatev1alpha1.AccessRule{{
		Name:            "proxy",
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    repositories,
		Permissions:     map[string]githubgatev1alpha1.Level{"contents": "read"},
		Proxy:           &githubgatev1alpha1.ProxyMode{Enabled: true, PublicRead: true},
	}}
	f.rebuild()

	return f
}

// publicReadMint decides a public read of other/public for ci-build and mints
// its token. It returns the repositories and permissions of the mint GitHub
// accepted for it.
func publicReadMint(t *testing.T, f *fixture) fakegithub.MintEntry {
	t.Helper()

	rules := f.store.Load().Rules(tenantNamespace, "ci-build")
	decision := proxy.Decide(rules, proxy.Decided{Request: proxy.Request{
		Host: proxy.HostAPI, Kind: proxy.KindREST, Method: http.MethodGet,
		Operation: "repos/get", Owner: "other", Repo: "public", Path: "/repos/other/public",
	}})
	require.True(t, decision.Allowed(), decision.Message)
	require.Equal(t, proxy.AccessPublicRead, decision.Access)

	before := len(f.fake.MintLog())

	token, status, err := f.server.ProxyToken(t.Context(),
		auth.Identity{Namespace: tenantNamespace, ServiceAccount: "ci-build"}, decision)
	require.NoError(t, err, "github status %s", status)
	require.NotEmpty(t, token)

	mints := f.fake.MintLog()
	require.Len(t, mints, before+1, "one accepted mint per token")

	return mints[len(mints)-1]
}

func TestPublicReadWorksWhileAnExactNameDoesNotResolve(t *testing.T) {
	f := publicReadFixture(t, "foo", "not-created-yet")

	mint := publicReadMint(t, f)
	assert.Equal(t, []string{"foo"}, mint.Repositories)
	assert.Equal(t, map[string]string{"metadata": "read"}, mint.Permissions)
}

func TestPublicReadUsesTheFirstSortedNameThatMints(t *testing.T) {
	f := publicReadFixture(t, "not-created-yet", "foo", "bar", "aaa-missing")

	mint := publicReadMint(t, f)
	assert.Equal(t, []string{"bar"}, mint.Repositories)
	assert.Equal(t, map[string]string{"metadata": "read"}, mint.Permissions)
}

func TestPublicReadMintsForEveryNameWhenAllResolve(t *testing.T) {
	f := publicReadFixture(t, "foo", "bar")

	mint := publicReadMint(t, f)
	assert.ElementsMatch(t, []string{"bar", "foo"}, mint.Repositories)
	assert.Equal(t, map[string]string{"metadata": "read"}, mint.Permissions)
}

func TestPublicReadFailsWhenNoNameResolves(t *testing.T) {
	f := publicReadFixture(t, "missing-one", "missing-two")

	rules := f.store.Load().Rules(tenantNamespace, "ci-build")
	decision := proxy.Decide(rules, proxy.Decided{Request: proxy.Request{
		Host: proxy.HostAPI, Kind: proxy.KindREST, Method: http.MethodGet,
		Operation: "repos/get", Owner: "other", Repo: "public", Path: "/repos/other/public",
	}})
	require.True(t, decision.Allowed(), decision.Message)

	_, _, err := f.server.ProxyToken(t.Context(),
		auth.Identity{Namespace: tenantNamespace, ServiceAccount: "ci-build"}, decision)

	var failure *proxy.TokenError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, http.StatusForbidden, failure.Status)
	assert.Empty(t, f.fake.MintLog())
}

func TestPublicReadRemembersTheNameThatMintedUntilItFails(t *testing.T) {
	f := publicReadFixture(t, "bar", "foo", "not-created-yet")

	assert.Equal(t, []string{"bar"}, publicReadMint(t, f).Repositories)

	// Every name now resolves, but the next mint goes straight to the
	// remembered one.
	require.NoError(t, f.fake.CreateRepository(owner, "not-created-yet", false))
	f.clock.advance(2 * time.Hour)
	assert.Equal(t, []string{"bar"}, publicReadMint(t, f).Repositories)

	// The remembered name stops resolving: the next mint forgets it and
	// falls back to the first name that mints.
	f.fake.DeleteInstallation(installationID)
	require.NoError(t, f.fake.CreateInstallation(fakegithub.Installation{
		ID: installationID, AppID: appID, Owner: owner,
		Permissions:  map[string]string{"contents": "write", "issues": "write"},
		Repositories: []string{"foo", "not-created-yet"},
	}))
	f.clock.advance(2 * time.Hour)
	assert.Equal(t, []string{"foo"}, publicReadMint(t, f).Repositories)
}
