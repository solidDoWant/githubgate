package model_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

func proxyRule(proxy githubgatev1alpha1.ProxyMode) githubgatev1alpha1.AccessRule {
	accessRule := rule("r", "main", contents("write"), "foo")
	accessRule.Token = nil
	proxy.Enabled = true
	accessRule.Proxy = &proxy

	return accessRule
}

func TestProxyRuleAcceptance(t *testing.T) {
	tests := []struct {
		name         string
		rule         githubgatev1alpha1.AccessRule
		wantAccepted bool
		wantMessage  string
	}{
		{
			name:         "a rule with only proxy mode is accepted",
			rule:         proxyRule(githubgatev1alpha1.ProxyMode{}),
			wantAccepted: true,
		},
		{
			name: "a rule whose lists match the catalogues is accepted",
			rule: proxyRule(githubgatev1alpha1.ProxyMode{
				PushRefs:        []githubgatev1alpha1.RefPattern{"refs/heads/agent/**"},
				AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/get", "issues/*"},
				DenyOperations:  []githubgatev1alpha1.OperationPattern{"repos/create-release"},
				AllowMutations:  []githubgatev1alpha1.MutationPattern{"createPullRequest", "add*"},
				DenyMutations:   []githubgatev1alpha1.MutationPattern{"mergePullRequest"},
			}),
			wantAccepted: true,
		},
		{
			name: "a rule with neither mode is an invalid spec",
			rule: func() githubgatev1alpha1.AccessRule {
				accessRule := proxyRule(githubgatev1alpha1.ProxyMode{})
				accessRule.Proxy.Enabled = false

				return accessRule
			}(),
			wantMessage: "no mode",
		},
		{
			name: "a rule with token mode and pushRefs is an invalid spec",
			rule: func() githubgatev1alpha1.AccessRule {
				accessRule := proxyRule(githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/x"}})
				accessRule.Token = &githubgatev1alpha1.TokenMode{Enabled: true}

				return accessRule
			}(),
			wantMessage: "token mode off",
		},
		{
			name: "a denied operation that matches nothing is an invalid spec naming it",
			rule: proxyRule(githubgatev1alpha1.ProxyMode{
				DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/create-release", "repos/no-such-operation"},
			}),
			wantMessage: `proxy.denyOperations[1]: "repos/no-such-operation"`,
		},
		{
			name:        "an allowed operation that matches nothing is an invalid spec naming it",
			rule:        proxyRule(githubgatev1alpha1.ProxyMode{AllowOperations: []githubgatev1alpha1.OperationPattern{"nothing/*"}}),
			wantMessage: `proxy.allowOperations[0]: "nothing/*"`,
		},
		{
			name:        "a denied mutation that matches nothing is an invalid spec naming it",
			rule:        proxyRule(githubgatev1alpha1.ProxyMode{DenyMutations: []githubgatev1alpha1.MutationPattern{"noSuchMutation"}}),
			wantMessage: `proxy.denyMutations[0]: "noSuchMutation"`,
		},
		{
			name:        "an allowed mutation that matches nothing is an invalid spec naming it",
			rule:        proxyRule(githubgatev1alpha1.ProxyMode{AllowMutations: []githubgatev1alpha1.MutationPattern{"zzz*"}}),
			wantMessage: `proxy.allowMutations[0]: "zzz*"`,
		},
		{
			name:        "a malformed ref pattern is an invalid spec naming it",
			rule:        proxyRule(githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/a..b"}}),
			wantMessage: `proxy.pushRefs[0]: "refs/heads/a..b"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := baseInputs()
			in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, test.rule)}

			snapshot := model.Build(in)

			decisions, ok := snapshot.PolicyRules(policyName(tenantNamespace))
			require.True(t, ok)
			require.Len(t, decisions, 1)
			assert.Equal(t, test.wantAccepted, decisions[0].Accepted, decisions[0].Message)

			rules := snapshot.Rules(tenantNamespace, "ci-build")
			if !test.wantAccepted {
				assert.Equal(t, model.ReasonInvalidSpec, decisions[0].Reason)
				assert.Contains(t, decisions[0].Message, test.wantMessage)
				assert.Empty(t, rules)

				return
			}

			require.Len(t, rules, 1)
			assert.False(t, rules[0].Token)
			require.NotNil(t, rules[0].Proxy)
			assert.Equal(t, test.rule.Proxy.PushRefs == nil, rules[0].Proxy.PushRefs == nil)
		})
	}
}

func TestRulesCarryTheirModes(t *testing.T) {
	in := baseInputs()
	tokenOnly := rule("token", "main", contents("read"), "foo")
	both := rule("both", "main", contents("read"), "foo")
	both.Proxy = &githubgatev1alpha1.ProxyMode{Enabled: true}
	proxyOnly := proxyRule(githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/create-release"}})
	proxyOnly.Name = "proxy"
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, tokenOnly, both, proxyOnly)}

	modes := map[string][2]bool{}
	for _, accepted := range model.Build(in).Rules(tenantNamespace, "ci-build") {
		modes[accepted.Name] = [2]bool{accepted.Token, accepted.Proxy != nil}

		if accepted.Name == "proxy" {
			assert.Equal(t, []string{"repos/create-release"}, accepted.Proxy.DenyOperations)
		}
	}

	assert.Equal(t, map[string][2]bool{"token": {true, false}, "both": {true, true}, "proxy": {false, true}}, modes)
}
