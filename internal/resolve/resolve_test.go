package resolve_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/resolve"
)

const adminNamespace = "githubgate"

var keyPEM = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
})

func app(name string, id int64, secret string) githubgatev1alpha1.GitHubApp {
	return githubgatev1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
		Spec: githubgatev1alpha1.GitHubAppSpec{
			AppID:               &id,
			PrivateKeySecretRef: githubgatev1alpha1.SecretKeyReference{Name: secret},
		},
	}
}

// installation admits the tenant namespaces used here and has the given
// ceiling, or a ceiling of contents, issues and pull_requests write on every
// repository when none is given.
func installation(name, appName, owner string, ceiling ...githubgatev1alpha1.PermissionSet) githubgatev1alpha1.GitHubInstallation {
	if len(ceiling) == 0 {
		ceiling = []githubgatev1alpha1.PermissionSet{{
			Repositories: []githubgatev1alpha1.Repository{"*"},
			Permissions: map[string]githubgatev1alpha1.Level{
				"contents": "write", "issues": "write", "pull_requests": "write",
			},
		}}
	}

	return githubgatev1alpha1.GitHubInstallation{
		ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
		Spec: githubgatev1alpha1.GitHubInstallationSpec{
			AppRef:            githubgatev1alpha1.ObjectReference{Name: appName},
			Owner:             owner,
			Ceiling:           ceiling,
			AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{"ci-foo", "ci-a", "ci-b"}},
		},
	}
}

type perms = map[string]githubgatev1alpha1.Level

func rule(name, installationName string, permissions perms, repositories ...githubgatev1alpha1.Repository) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: installationName},
		Repositories:    repositories,
		Permissions:     permissions,
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}

func policy(namespace, name string, serviceAccounts []string, rules ...githubgatev1alpha1.AccessRule) githubgatev1alpha1.GitHubAccessPolicy {
	return githubgatev1alpha1.GitHubAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
			ServiceAccounts: serviceAccounts,
			Rules:           rules,
		},
	}
}

// snapshot has two ready Apps, each installed on solidDoWant (main through
// app-one, second through app-two), app-one also installed on other-owner,
// an installation limited to repository a, and one whose App has no key.
func snapshot(policies ...githubgatev1alpha1.GitHubAccessPolicy) *model.Snapshot {
	secret := func(name string) corev1.Secret {
		return corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
			Data:       map[string][]byte{model.DefaultPrivateKeyKey: keyPEM()},
		}
	}

	return model.Build(model.Inputs{
		AdminNamespaces: []string{adminNamespace},
		Apps: []githubgatev1alpha1.GitHubApp{
			app("app-one", 1, "key-one"),
			app("app-two", 2, "key-two"),
			app("app-keyless", 3, "missing"),
		},
		Installations: []githubgatev1alpha1.GitHubInstallation{
			installation("main", "app-one", "solidDoWant"),
			installation("second", "app-two", "solidDoWant"),
			installation("other", "app-one", "other-owner"),
			installation("broken", "app-keyless", "broken-owner"),
			installation("limited", "app-one", "limited-owner", githubgatev1alpha1.PermissionSet{
				Repositories: []githubgatev1alpha1.Repository{"a"},
				Permissions:  perms{"contents": "write"},
			}),
		},
		Secrets:  []corev1.Secret{secret("key-one"), secret("key-two")},
		Policies: policies,
		Namespaces: []corev1.Namespace{
			{ObjectMeta: metav1.ObjectMeta{Name: adminNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ci-foo"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ci-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ci-b"}},
		},
	})
}

func caller(namespace, serviceAccount string) resolve.Identity {
	return resolve.Identity{Namespace: namespace, ServiceAccount: serviceAccount}
}

func levels(set map[string]string) map[string]permissions.Level {
	result := map[string]permissions.Level{}
	for name, level := range set {
		result[name] = permissions.Level(level)
	}

	return result
}

// ciFoo is the SPEC's example: ci-build may read foo, ci-release may write it.
var ciFoo = []githubgatev1alpha1.GitHubAccessPolicy{
	policy("ci-foo", "ci-build", []string{"ci-build"}, rule("read", "main", perms{"contents": "read"}, "foo")),
	policy("ci-foo", "ci-release", []string{"ci-release"}, rule("release", "main", perms{"contents": "write"}, "foo")),
}

func manyRepositories(count int) []string {
	names := make([]string, 0, count)
	for index := range count {
		names = append(names, fmt.Sprintf("repo-%d", index))
	}

	return names
}

func TestResolve(t *testing.T) {
	type expect struct {
		outcome      resolve.Outcome
		installation string
		repositories []string
		permissions  map[string]string
		rules        []string
		// message lists text every refusal message must contain.
		message []string
	}

	tests := []struct {
		name     string
		policies []githubgatev1alpha1.GitHubAccessPolicy
		caller   resolve.Identity
		request  resolve.Request
		want     expect
	}{
		{
			name:     "a named ServiceAccount gets exactly what its policy gives on one repository",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/ci-build/read"},
			},
		},
		{
			name:     "repository names match rules case-insensitively",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"FOO"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"FOO"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/ci-build/read"},
			},
		},
		{
			name:     "a ServiceAccount no policy names is refused, naming its namespace and ServiceAccount",
			policies: ciFoo,
			caller:   caller("ci-foo", "stranger"),
			request:  resolve.Request{Repositories: []string{"foo"}},
			want:     expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"stranger"`}},
		},
		{
			name:     "a repository the policy doesn't cover is refused, naming the repository",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"foo", "bar"}},
			want:     expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, `repository "bar"`}},
		},
		{
			name:     "a higher level than the policy gives is refused, naming the permission",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "write"}),
			},
			want: expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, "permission contents: write"}},
		},
		{
			name:     "a permission the policy doesn't give is refused, naming the permission",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "read", "issues": "read"}),
			},
			want: expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, "permission issues: read"}},
		},
		{
			name:     "a requested lower level than the policy gives is granted as requested",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-release"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "read"}),
			},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/ci-release/release"},
			},
		},
		{
			name: "two repositories with different grants and no permissions get only what both are given, at the lower level",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("foo", "main", perms{"contents": "write", "issues": "write"}, "foo"),
				rule("bar", "main", perms{"contents": "read", "pull_requests": "read"}, "bar"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: []string{"foo", "bar"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo", "bar"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/foo", "ci-foo/p/bar"},
			},
		},
		{
			name: "within one repository the highest level of its rules counts",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("glob", "main", perms{"contents": "read", "issues": "write"}, "fo*"),
				rule("exact", "main", perms{"contents": "write"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "write", "issues": "write"},
				rules: []string{"ci-foo/p/glob", "ci-foo/p/exact"},
			},
		},
		{
			name: "a requested permission must be given on every repository",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("foo", "main", perms{"contents": "write"}, "foo"),
				rule("bar", "main", perms{"contents": "read"}, "bar"),
			)},
			caller: caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo", "bar"},
				Permissions:  levels(map[string]string{"contents": "write"}),
			},
			want: expect{outcome: resolve.OutcomeDenied, message: []string{"permission contents: write", `repository "bar"`}},
		},
		{
			name: "repositories that share no permission are refused",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("foo", "main", perms{"issues": "write"}, "foo"),
				rule("bar", "main", perms{"contents": "read"}, "bar"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: []string{"foo", "bar"}},
			want:    expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, `"foo"`, `"bar"`, "share no permission"}},
		},
		{
			name: "with no repositories named the token covers the repositories the rules name exactly",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("foo", "main", perms{"contents": "write"}, "foo", "bar-*"),
				rule("baz", "main", perms{"contents": "read"}, "baz", "FOO"),
			)},
			caller: caller("ci-foo", "ci-build"),
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"baz", "foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/foo", "ci-foo/p/baz"},
			},
		},
		{
			name: "with no repositories named and only glob rules the request is refused asking for repositories",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("glob", "main", perms{"contents": "read"}, "foo-*", "*-bar"),
			)},
			caller: caller("ci-foo", "ci-build"),
			want:   expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`"ci-foo"`, `"ci-build"`, "name the repositories"}},
		},
		{
			name:     "a pull-request ServiceAccount can't get the write access a release needs",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "write"}),
			},
			want: expect{outcome: resolve.OutcomeDenied, message: []string{"permission contents: write"}},
		},
		{
			name:     "the release ServiceAccount in the same namespace gets write access",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-release"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "write"}),
			},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "write"},
				rules: []string{"ci-foo/ci-release/release"},
			},
		},
		{
			name: "a rule outside its installation's ceiling grants nothing",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-a", "p", []string{"ci-build"},
				rule("b", "limited", perms{"contents": "read"}, "b"),
			)},
			caller:  caller("ci-a", "ci-build"),
			request: resolve.Request{Repositories: []string{"b"}},
			want:    expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-a"`, `"ci-build"`}},
		},
		{
			name: "a rule outside the ceiling grants nothing while the caller's other rules still work",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-a", "p", []string{"ci-build"},
				rule("a", "limited", perms{"contents": "read"}, "a"),
				rule("b", "limited", perms{"contents": "read"}, "b"),
			)},
			caller:  caller("ci-a", "ci-build"),
			request: resolve.Request{Repositories: []string{"a"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/limited",
				repositories: []string{"a"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-a/p/a"},
			},
		},
		{
			name: "a ServiceAccount of another namespace gets nothing from a policy naming the same ServiceAccount name",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-a", "p", []string{"ci-build"},
				rule("foo", "main", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-b", "ci-build"),
			request: resolve.Request{Repositories: []string{"foo"}},
			want:    expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-b"`, `"ci-build"`}},
		},
		{
			name: "a caller allowed through two installations of one owner must name the installation",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("one", "main", perms{"contents": "read"}, "foo"),
				rule("two", "second", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Owner: "soliddowant", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAmbiguousInstallation,
				message: []string{`"ci-foo"`, `"ci-build"`, "githubgate/main", "githubgate/second"},
			},
		},
		{
			name: "a caller allowed through two installations gets a token from the one it names",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("one", "main", perms{"contents": "read"}, "foo"),
				rule("two", "second", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Installation: "githubgate/second", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/second",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/two"},
			},
		},
		{
			name: "a caller allowed through one of two installations of an owner gets a token from that one",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("two", "second", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Owner: "solidDoWant", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/second",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/two"},
			},
		},
		{
			name: "only the installation whose rules cover the request is a candidate",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("one", "main", perms{"contents": "read"}, "foo"),
				rule("two", "second", perms{"contents": "read"}, "bar"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Owner: "solidDoWant", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/one"},
			},
		},
		{
			name: "the owner limits the candidate installations",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("one", "main", perms{"contents": "read"}, "foo"),
				rule("other", "other", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Owner: "Other-Owner", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/other",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/other"},
			},
		},
		{
			name:     "an owner the caller has no rule for is refused",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Owner: "other-owner", Repositories: []string{"foo"}},
			want:     expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, `"other-owner"`}},
		},
		{
			name:     "a named installation the caller has no rule for is refused",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Installation: "githubgate/second", Repositories: []string{"foo"}},
			want:     expect{outcome: resolve.OutcomeDenied, message: []string{`"ci-foo"`, `"ci-build"`, "githubgate/second"}},
		},
		{
			name:     "a named installation with an owner that isn't its own is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Installation: "githubgate/main", Owner: "other-owner", Repositories: []string{"foo"}},
			want:     expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`"ci-foo"`, `"ci-build"`, `"other-owner"`}},
		},
		{
			name:     "a named installation with its own owner in another case is allowed",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Installation: "githubgate/main", Owner: "SOLIDDOWANT", Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/ci-build/read"},
			},
		},
		{
			name:     "an installation that isn't namespace/name is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Installation: "main", Repositories: []string{"foo"}},
			want:     expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`"ci-foo"`, `"ci-build"`, `"main"`}},
		},
		{
			name: "an installation that isn't ready is unavailable",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("broken", "broken", perms{"contents": "read"}, "foo"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: []string{"foo"}},
			want: expect{
				outcome: resolve.OutcomeUnavailable, installation: "githubgate/broken",
				message: []string{`"ci-foo"`, `"ci-build"`, "githubgate/broken"},
			},
		},
		{
			name:     "a repository with an owner prefix is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"solidDoWant/foo"}},
			want:     expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`"ci-foo"`, `"ci-build"`, `"solidDoWant/foo"`}},
		},
		{
			name:     "a repository pattern is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"fo*"}},
			want:     expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`"fo*"`}},
		},
		{
			name:     "the repository name .. is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{".."}},
			want:     expect{outcome: resolve.OutcomeInvalidRequest, message: []string{`".."`}},
		},
		{
			name:     "an unknown permission is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"no_such_permission": "read"}),
			},
			want: expect{outcome: resolve.OutcomeInvalidRequest, message: []string{"no_such_permission"}},
		},
		{
			name:     "an organization permission is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"members": "read"}),
			},
			want: expect{outcome: resolve.OutcomeInvalidRequest, message: []string{"members"}},
		},
		{
			name:     "a level the permission doesn't have is an invalid request",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request: resolve.Request{
				Repositories: []string{"foo"},
				Permissions:  levels(map[string]string{"contents": "admin"}),
			},
			want: expect{outcome: resolve.OutcomeInvalidRequest, message: []string{"contents: admin"}},
		},
		{
			name:     "duplicate repositories are removed case-insensitively",
			policies: ciFoo,
			caller:   caller("ci-foo", "ci-build"),
			request:  resolve.Request{Repositories: []string{"foo", "Foo", "FOO"}},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: []string{"foo"}, permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/ci-build/read"},
			},
		},
		{
			name: "the most repositories GitHub accepts in one token are allowed",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("all", "main", perms{"contents": "read"}, "repo-*"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: manyRepositories(500)},
			want: expect{
				outcome: resolve.OutcomeAllowed, installation: "githubgate/main",
				repositories: manyRepositories(500), permissions: map[string]string{"contents": "read"},
				rules: []string{"ci-foo/p/all"},
			},
		},
		{
			name: "one repository more than GitHub accepts is an invalid request",
			policies: []githubgatev1alpha1.GitHubAccessPolicy{policy("ci-foo", "p", []string{"ci-build"},
				rule("all", "main", perms{"contents": "read"}, "repo-*"),
			)},
			caller:  caller("ci-foo", "ci-build"),
			request: resolve.Request{Repositories: manyRepositories(501)},
			want:    expect{outcome: resolve.OutcomeInvalidRequest, message: []string{"501"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := resolve.Resolve(test.caller, test.request, snapshot(test.policies...))

			require.Equal(t, test.want.outcome, decision.Outcome, decision.Message)

			if test.want.installation == "" {
				assert.Nil(t, decision.Installation)
			} else {
				require.NotNil(t, decision.Installation)
				assert.Equal(t, test.want.installation, decision.Installation.String())
			}

			if test.want.outcome != resolve.OutcomeAllowed {
				assert.Empty(t, decision.Repositories)
				assert.Empty(t, decision.Permissions)
				assert.Empty(t, decision.Rules)

				for _, text := range test.want.message {
					assert.Contains(t, decision.Message, text)
				}

				return
			}

			assert.Equal(t, test.want.repositories, decision.Repositories)
			assert.Equal(t, levels(test.want.permissions), decision.Permissions)

			rules := make([]string, 0, len(decision.Rules))
			for _, matched := range decision.Rules {
				rules = append(rules, matched.Policy.String()+"/"+matched.Name)
			}

			assert.ElementsMatch(t, test.want.rules, rules)
		})
	}
}

func TestOutcomeStatus(t *testing.T) {
	tests := []struct {
		outcome resolve.Outcome
		status  int
	}{
		{outcome: resolve.OutcomeAllowed, status: http.StatusOK},
		{outcome: resolve.OutcomeInvalidRequest, status: http.StatusBadRequest},
		{outcome: resolve.OutcomeDenied, status: http.StatusForbidden},
		{outcome: resolve.OutcomeAmbiguousInstallation, status: http.StatusConflict},
		{outcome: resolve.OutcomeUnavailable, status: http.StatusServiceUnavailable},
		{outcome: resolve.Outcome("something else"), status: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(string(test.outcome), func(t *testing.T) {
			assert.Equal(t, test.status, test.outcome.Status())
		})
	}
}
