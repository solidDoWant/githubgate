package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

var (
	app1Installation = &model.Installation{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "app1-acme"}, Owner: "acme", Ready: true,
	}
	app2Installation = &model.Installation{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "app2-acme"}, Owner: "acme", Ready: true,
	}
)

type ruleOption func(*model.Rule)

func proxyRule(name string, installation *model.Installation, repositories []string, set map[string]permissions.Level, options ...ruleOption) *model.Rule {
	rule := &model.Rule{
		Policy: types.NamespacedName{Namespace: "ci-a", Name: "policy"}, Name: name,
		Installation: installation, Repositories: repositories, Permissions: set, Proxy: &model.ProxyRule{},
	}

	for _, option := range options {
		option(rule)
	}

	return rule
}

func denyOperations(ids ...string) ruleOption {
	return func(rule *model.Rule) { rule.Proxy.DenyOperations = ids }
}

func allowOperations(ids ...string) ruleOption {
	return func(rule *model.Rule) { rule.Proxy.AllowOperations = ids }
}

func denyMutations(names ...string) ruleOption {
	return func(rule *model.Rule) { rule.Proxy.DenyMutations = names }
}

func allowMutations(names ...string) ruleOption {
	return func(rule *model.Rule) { rule.Proxy.AllowMutations = names }
}

func pushRefs(refs ...string) ruleOption {
	return func(rule *model.Rule) { rule.Proxy.PushRefs = refs }
}

func tokenOnly() ruleOption {
	return func(rule *model.Rule) { rule.Proxy, rule.Token = nil, true }
}

func write(name string) map[string]permissions.Level {
	return map[string]permissions.Level{name: permissions.LevelWrite}
}

func contentsAndDeployments() map[string]permissions.Level {
	return map[string]permissions.Level{"contents": permissions.LevelWrite, "deployments": permissions.LevelWrite}
}

func read(name string) map[string]permissions.Level {
	return map[string]permissions.Level{name: permissions.LevelRead}
}

func rest(operation, owner, repo string) proxy.Decided {
	return proxy.Decided{Request: proxy.Request{Host: proxy.HostAPI, Kind: proxy.KindREST, Operation: operation, Owner: owner, Repo: repo}}
}

func mutation(names ...string) proxy.Decided {
	return proxy.Decided{Request: proxy.Request{Host: proxy.HostAPI, Kind: proxy.KindGraphQL}, Mutations: names}
}

func git(kind proxy.Kind, repo string, contents permissions.Level) proxy.Decided {
	return proxy.Decided{Request: proxy.Request{Host: proxy.HostGit, Kind: kind, Owner: "acme", Repo: repo, Contents: contents}}
}

func TestDecideAllowsOnlyWhatAllowingRulesGive(t *testing.T) {
	tests := []struct {
		name                 string
		rules                []*model.Rule
		request              proxy.Decided
		expectedStatus       int
		expectedCode         string
		expectedMessage      []string
		expectedInstallation *model.Installation
		expectedRepositories []string
		expectedPermissions  map[string]permissions.Level
	}{
		{
			name: "a release denied by one rule gets only the other rule's issues permission",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, write("contents"), denyOperations("repos/create-release")),
				proxyRule("b", app1Installation, []string{"foo"}, write("issues")),
			},
			request:              rest("repos/create-release", "acme", "foo"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("issues"),
		},
		{
			name: "a push outside pushRefs is not given by a rule with only contents read",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**")),
				proxyRule("b", app1Installation, []string{"foo"}, read("contents")),
			},
			request: func() proxy.Decided {
				request := git(proxy.KindGitReceive, "foo", permissions.LevelWrite)
				request.Refs = []string{"refs/heads/master"}

				return request
			}(),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"acme/foo"`, "refs/heads/master", "contents: write"},
		},
		{
			name: "a denied mutation is allowed only through the other rule's repositories",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, write("pull_requests"), denyMutations("mergePullRequest")),
				proxyRule("b", app1Installation, []string{"bar"}, write("pull_requests")),
			},
			request:              mutation("mergePullRequest"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"bar"},
			expectedPermissions:  write("pull_requests"),
		},
		{
			name:  "updating a ref outside pushRefs is refused naming the ref",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request: func() proxy.Decided {
				request := rest("git/update-ref", "acme", "foo")
				request.Refs = []string{"refs/heads/master"}

				return request
			}(),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"refs/heads/master"`, "pushRefs"},
		},
		{
			name:  "updating a ref inside pushRefs is allowed",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request: func() proxy.Decided {
				request := rest("git/update-ref", "acme", "foo")
				request.Refs = []string{"refs/heads/agent/x"}

				return request
			}(),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("contents"),
		},
		{
			name:            "a ref-writing mutation is refused under pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))},
			request:         mutation("updateRefs"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"updateRefs"`},
		},
		{
			name:            "creating a deployment is refused under pushRefs because it can auto-merge into its ref",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, contentsAndDeployments(), pushRefs("refs/heads/agent/**"))},
			request:         rest("repos/create-deployment", "acme", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"repos/create-deployment"`, "pushRefs"},
		},
		{
			name:            "the createDeployment mutation is refused under pushRefs",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, contentsAndDeployments(), pushRefs("refs/heads/agent/**"))},
			request:         mutation("createDeployment"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"createDeployment"`},
		},
		{
			name:                 "creating a deployment is allowed without pushRefs",
			rules:                []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, contentsAndDeployments())},
			request:              rest("repos/create-deployment", "acme", "foo"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  contentsAndDeployments(),
		},
		{
			name: "a clone uses the only installation whose rule covers the repository",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, read("contents")),
				proxyRule("b", app2Installation, []string{"bar"}, read("contents")),
			},
			request:              git(proxy.KindGitUpload, "foo", permissions.LevelRead),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  read("contents"),
		},
		{
			name:            "a repository no rule covers is refused naming it",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"))},
			request:         rest("repos/get", "acme", "other"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"acme/other"`},
		},
		{
			name:            "a rule for another owner doesn't cover a repository of the same name",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"))},
			request:         rest("repos/get", "someone-else", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"someone-else/foo"`},
		},
		{
			name:            "a rule with only token mode gives nothing through the proxy",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"), tokenOnly())},
			request:         rest("repos/get", "acme", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"acme/foo"`},
		},
		{
			name:            "a denied operation is refused naming it",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), denyOperations("repos/create-release"))},
			request:         rest("repos/create-release", "acme", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"repos/create-release"`},
		},
		{
			name:                 "an operation the deny list doesn't name is allowed",
			rules:                []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("issues"), denyOperations("repos/create-release"))},
			request:              rest("issues/create-comment", "acme", "foo"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("issues"),
		},
		{
			name:                 "a listed operation is allowed",
			rules:                []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("issues"), allowOperations("issues/*"))},
			request:              rest("issues/create-comment", "acme", "foo"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("issues"),
		},
		{
			name:            "an operation missing from allowOperations is refused naming it",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("issues"), allowOperations("issues/*"))},
			request:         rest("repos/get", "acme", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"repos/get"`},
		},
		{
			name:            "deny wins over allow",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("issues"), allowOperations("issues/*"), denyOperations("issues/create-comment"))},
			request:         rest("issues/create-comment", "acme", "foo"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"issues/create-comment"`},
		},
		{
			name:            "a mutation missing from allowMutations is refused naming it",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("pull_requests"), allowMutations("createPullRequest"))},
			request:         mutation("createPullRequest", "mergePullRequest"),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"mergePullRequest"`},
		},
		{
			name:                 "a listed mutation is allowed",
			rules:                []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("pull_requests"), allowMutations("createPullRequest"))},
			request:              mutation("createPullRequest"),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("pull_requests"),
		},
		{
			name:            "GraphQL through rules that only use patterns is refused",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo-*"}, read("contents"))},
			request:         mutation(),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{"exact repository names"},
		},
		{
			name: "GraphQL gets the exact repositories with their lowest common level",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo", "foo-*"}, map[string]permissions.Level{"contents": permissions.LevelWrite, "issues": permissions.LevelRead}),
				proxyRule("b", app1Installation, []string{"bar"}, read("contents")),
			},
			request:              mutation(),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"bar", "foo"},
			expectedPermissions:  read("contents"),
		},
		{
			name: "GraphQL through two installations is ambiguous",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, read("contents")),
				proxyRule("b", app2Installation, []string{"bar"}, read("contents")),
			},
			request:         mutation(),
			expectedStatus:  http.StatusConflict,
			expectedCode:    "ambiguous_installation",
			expectedMessage: []string{"githubgate/app1-acme", "githubgate/app2-acme"},
		},
		{
			name: "a repository allowed through two installations is ambiguous",
			rules: []*model.Rule{
				proxyRule("a", app1Installation, []string{"foo"}, read("contents")),
				proxyRule("b", app2Installation, []string{"foo"}, read("contents")),
			},
			request:        rest("repos/get", "acme", "foo"),
			expectedStatus: http.StatusConflict,
			expectedCode:   "ambiguous_installation",
		},
		{
			name:            "REST without a repository needs allowOperations",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"))},
			request:         rest("users/get-authenticated", "", ""),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{`"users/get-authenticated"`, "allowOperations"},
		},
		{
			name:                 "REST without a repository in allowOperations is allowed",
			rules:                []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("contents"), allowOperations("users/get-authenticated"))},
			request:              rest("users/get-authenticated", "", ""),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  read("contents"),
		},
		{
			name:            "a git fetch needs contents read",
			rules:           []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, read("issues"))},
			request:         git(proxy.KindGitUpload, "foo", permissions.LevelRead),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{"contents: read"},
		},
		{
			name:  "a push without pushRefs needs only contents write",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"))},
			request: func() proxy.Decided {
				request := git(proxy.KindGitReceive, "foo", permissions.LevelWrite)
				request.RefsUnknown = true

				return request
			}(),
			expectedInstallation: app1Installation,
			expectedRepositories: []string{"foo"},
			expectedPermissions:  write("contents"),
		},
		{
			name:  "a push whose refs weren't read is refused under pushRefs",
			rules: []*model.Rule{proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/**"))},
			request: func() proxy.Decided {
				request := git(proxy.KindGitReceive, "foo", permissions.LevelWrite)
				request.RefsUnknown = true

				return request
			}(),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: []string{"pushRefs"},
		},
		{
			name: "an installation that isn't ready is unavailable",
			rules: []*model.Rule{proxyRule("a", &model.Installation{
				NamespacedName: app1Installation.NamespacedName, Owner: "acme", Reason: "AppNotReady",
			}, []string{"foo"}, read("contents"))},
			request:        rest("repos/get", "acme", "foo"),
			expectedStatus: http.StatusServiceUnavailable,
			expectedCode:   "unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := proxy.Decide(test.rules, test.request)

			if test.expectedStatus != 0 {
				require.False(t, decision.Allowed(), "the request was allowed")
				assert.Equal(t, test.expectedStatus, decision.Status)
				assert.Equal(t, test.expectedCode, decision.Code)

				for _, part := range test.expectedMessage {
					assert.Contains(t, decision.Message, part)
				}

				return
			}

			require.True(t, decision.Allowed(), decision.Message)
			assert.Same(t, test.expectedInstallation, decision.Installation)
			assert.Equal(t, test.expectedRepositories, decision.Repositories)
			assert.Equal(t, test.expectedPermissions, decision.Permissions)
		})
	}
}
