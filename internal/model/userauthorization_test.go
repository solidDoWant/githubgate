package model_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	createSigningKey = "users/create-ssh-signing-key-for-authenticated-user"
	deleteSigningKey = "users/delete-ssh-signing-key-for-authenticated-user"
)

func userAuthorization(namespace, name string, operations ...githubgatev1alpha1.OperationName) githubgatev1alpha1.GitHubUserAuthorization {
	return githubgatev1alpha1.GitHubUserAuthorization{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: githubgatev1alpha1.GitHubUserAuthorizationSpec{
			AppRef:            githubgatev1alpha1.ObjectReference{Name: "app"},
			Login:             "octocat",
			Ceiling:           githubgatev1alpha1.UserCeiling{Operations: operations},
			AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
		},
	}
}

func userRule(name, authorization string, operations ...githubgatev1alpha1.OperationPattern) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:                 name,
		UserAuthorizationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: authorization},
		Proxy:                &githubgatev1alpha1.ProxyMode{Enabled: true, AllowOperations: operations},
	}
}

// userInputs adds to baseInputs an App client ID given inline and an
// authorization "user" whose ceiling is the signing-key create and delete
// operations, admitting ci-a.
func userInputs() model.Inputs {
	in := baseInputs()
	in.Apps[0].Spec.ClientID = "Iv23liAbCdEf012345"
	in.UserAuthorizations = []githubgatev1alpha1.GitHubUserAuthorization{
		userAuthorization(adminNamespace, "user", createSigningKey, deleteSigningKey),
	}

	return in
}

func clientIDSecret(data map[string][]byte) corev1.Secret {
	return corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "app-ids"}, Data: data}
}

func TestUserAuthorizationResolution(t *testing.T) {
	tests := []struct {
		name         string
		modify       func(*model.Inputs)
		wantResolved bool
		wantReason   string
		wantClientID string
	}{
		{
			name:         "an inline client ID resolves",
			wantResolved: true,
			wantReason:   model.ReasonReady,
			wantClientID: "Iv23liAbCdEf012345",
		},
		{
			name: "a client ID from a Secret's default key resolves",
			modify: func(in *model.Inputs) {
				in.Apps[0].Spec.ClientID = ""
				in.Apps[0].Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-ids"}
				in.Secrets = append(in.Secrets, clientIDSecret(map[string][]byte{"client-id": []byte("Iv23liFromSecret\n")}))
			},
			wantResolved: true,
			wantReason:   model.ReasonReady,
			wantClientID: "Iv23liFromSecret",
		},
		{
			name: "a client ID from a Secret's named key resolves",
			modify: func(in *model.Inputs) {
				in.Apps[0].Spec.ClientID = ""
				in.Apps[0].Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-ids", Key: "oauth"}
				in.Secrets = append(in.Secrets, clientIDSecret(map[string][]byte{"oauth": []byte("Iv23liNamedKey")}))
			},
			wantResolved: true,
			wantReason:   model.ReasonReady,
			wantClientID: "Iv23liNamedKey",
		},
		{
			name: "a missing client ID Secret is unavailable",
			modify: func(in *model.Inputs) {
				in.Apps[0].Spec.ClientID = ""
				in.Apps[0].Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-ids"}
			},
			wantReason: model.ReasonSecretUnavailable,
		},
		{
			name: "a client ID Secret without the key is unavailable",
			modify: func(in *model.Inputs) {
				in.Apps[0].Spec.ClientID = ""
				in.Apps[0].Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-ids"}
				in.Secrets = append(in.Secrets, clientIDSecret(map[string][]byte{"app-id": []byte("42")}))
			},
			wantReason: model.ReasonSecretUnavailable,
		},
		{
			name: "an empty client ID in the Secret is invalid",
			modify: func(in *model.Inputs) {
				in.Apps[0].Spec.ClientID = ""
				in.Apps[0].Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-ids"}
				in.Secrets = append(in.Secrets, clientIDSecret(map[string][]byte{"client-id": []byte(" \n")}))
			},
			wantReason: model.ReasonSecretInvalid,
		},
		{
			name:       "an App without a client ID is an invalid spec",
			modify:     func(in *model.Inputs) { in.Apps[0].Spec.ClientID = "" },
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "a missing App is not found",
			modify:     func(in *model.Inputs) { in.UserAuthorizations[0].Spec.AppRef.Name = "missing" },
			wantReason: model.ReasonRefNotFound,
		},
		{
			name: "an App that doesn't admit the namespace is not allowed",
			modify: func(in *model.Inputs) {
				in.Namespaces = append(in.Namespaces, namespace("other-admin", nil))
				in.AdminNamespaces = append(in.AdminNamespaces, "other-admin")
				in.UserAuthorizations[0].Namespace = "other-admin"
				in.UserAuthorizations[0].Spec.AppRef.Namespace = adminNamespace
			},
			wantReason: model.ReasonNotAllowed,
		},
		{
			name:       "an App that isn't ready makes the authorization not ready",
			modify:     func(in *model.Inputs) { in.Secrets = nil },
			wantReason: model.ReasonRefNotReady,
		},
		{
			name: "a ceiling operation that names a repository is an invalid spec",
			modify: func(in *model.Inputs) {
				in.UserAuthorizations[0].Spec.Ceiling.Operations = append(in.UserAuthorizations[0].Spec.Ceiling.Operations, "repos/get")
			},
			wantReason: model.ReasonInvalidSpec,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := userInputs()
			if test.modify != nil {
				test.modify(&in)
			}

			snapshot := model.Build(in)

			name := types.NamespacedName{Namespace: in.UserAuthorizations[0].Namespace, Name: "user"}
			authorization, ok := snapshot.UserAuthorization(name)
			require.True(t, ok)
			assert.Equal(t, test.wantResolved, authorization.Resolved)
			assert.Equal(t, test.wantReason, authorization.Reason)
			assert.Equal(t, test.wantClientID, authorization.ClientID)

			if test.wantReason != model.ReasonRefNotReady {
				app, ok := snapshot.App(types.NamespacedName{Namespace: adminNamespace, Name: "app"})
				require.True(t, ok)
				assert.True(t, app.Ready, "a missing or broken client ID never makes the App not ready")
			}
		})
	}
}

func TestUserAuthorizationOutsideAdminNamespacesIsIgnored(t *testing.T) {
	in := userInputs()
	in.UserAuthorizations[0].Namespace = tenantNamespace

	_, ok := model.Build(in).UserAuthorization(types.NamespacedName{Namespace: tenantNamespace, Name: "user"})
	assert.False(t, ok)
}

func TestUserRuleAcceptance(t *testing.T) {
	tests := []struct {
		name            string
		modify          func(*model.Inputs)
		policyNamespace string
		rule            githubgatev1alpha1.AccessRule
		wantAccepted    bool
		wantReason      string
	}{
		{
			name:         "a user rule inside the ceiling is accepted",
			rule:         userRule("r", "user", createSigningKey, deleteSigningKey),
			wantAccepted: true,
			wantReason:   model.ReasonAccepted,
		},
		{
			name:       "an operation outside the ceiling is outside the ceiling",
			rule:       userRule("r", "user", createSigningKey, "users/list-ssh-signing-keys-for-authenticated-user"),
			wantReason: model.ReasonOutsideCeiling,
		},
		{
			name:            "a namespace the authorization doesn't admit is not allowed",
			policyNamespace: "ci-b",
			rule:            userRule("r", "user", createSigningKey),
			wantReason:      model.ReasonNotAllowed,
		},
		{
			name: "a namespace admitted by the authorization's selector is accepted",
			modify: func(in *model.Inputs) {
				in.UserAuthorizations[0].Spec.AllowedNamespaces = githubgatev1alpha1.AllowedNamespaces{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
				}
			},
			policyNamespace: "ci-b",
			rule:            userRule("r", "user", createSigningKey),
			wantAccepted:    true,
			wantReason:      model.ReasonAccepted,
		},
		{
			name:       "a missing authorization is not found",
			rule:       userRule("r", "missing", createSigningKey),
			wantReason: model.ReasonRefNotFound,
		},
		{
			name:         "an authorization whose App has no client ID still accepts the rule as not ready",
			modify:       func(in *model.Inputs) { in.Apps[0].Spec.ClientID = "" },
			rule:         userRule("r", "user", createSigningKey),
			wantAccepted: true,
			wantReason:   model.ReasonRefNotReady,
		},
		{
			name:       "an operation that names a repository is an invalid spec",
			rule:       userRule("r", "user", "repos/get"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "a user rule with no operations is an invalid spec",
			rule:       userRule("r", "user"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name: "a user rule with token mode is an invalid spec",
			rule: func() githubgatev1alpha1.AccessRule {
				r := userRule("r", "user", createSigningKey)
				r.Token = &githubgatev1alpha1.TokenMode{Enabled: true}

				return r
			}(),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name: "a user rule with an installation is an invalid spec",
			rule: func() githubgatev1alpha1.AccessRule {
				r := userRule("r", "user", createSigningKey)
				r.InstallationRef = &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"}

				return r
			}(),
			wantReason: model.ReasonInvalidSpec,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := userInputs()
			if test.modify != nil {
				test.modify(&in)
			}

			policyNamespace := test.policyNamespace
			if policyNamespace == "" {
				policyNamespace = tenantNamespace
			}

			in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(policyNamespace, test.rule)}

			snapshot := model.Build(in)

			decisions, ok := snapshot.PolicyRules(policyName(policyNamespace))
			require.True(t, ok)
			require.Len(t, decisions, 1)
			assert.Equal(t, test.wantAccepted, decisions[0].Accepted)
			assert.Equal(t, test.wantReason, decisions[0].Reason, decisions[0].Message)
		})
	}
}

func TestAcceptedUserRuleIsNeverServedAsAnInstallationRule(t *testing.T) {
	in := userInputs()
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, userRule("r", "user", createSigningKey))}

	snapshot := model.Build(in)

	decisions, _ := snapshot.PolicyRules(policyName(tenantNamespace))
	require.Len(t, decisions, 1)
	require.True(t, decisions[0].Accepted)

	rules := snapshot.Rules(tenantNamespace, "ci-build")
	require.Len(t, rules, 1)
	assert.Nil(t, rules[0].Installation, "a user rule has no installation")
	assert.Nil(t, rules[0].Proxy, "a user rule is not an installation proxy rule")
	assert.False(t, rules[0].Token, "a user rule is not a token rule")
	require.NotNil(t, rules[0].User)
	assert.Equal(t, []string{createSigningKey}, rules[0].User.Operations)
	assert.True(t, rules[0].User.Accepted)
}

func TestUserRulesReachTheCallerAcceptedOrNot(t *testing.T) {
	outside := userRule("outside", "user", "users/list-emails-for-authenticated-user")
	in := userInputs()
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, userRule("inside", "user", createSigningKey), outside)}

	snapshot := model.Build(in)

	rules := snapshot.Rules(tenantNamespace, "ci-build")
	require.Len(t, rules, 2)

	byName := map[string]*model.UserRule{}

	for _, rule := range rules {
		require.NotNil(t, rule.User)
		byName[rule.Name] = rule.User
	}

	assert.True(t, byName["inside"].Accepted)
	assert.False(t, byName["outside"].Accepted)
	assert.Equal(t, model.ReasonOutsideCeiling, byName["outside"].Reason)
	assert.Equal(t, []string{"users/list-emails-for-authenticated-user"}, byName["outside"].Operations)
	require.NotNil(t, byName["inside"].View)
	assert.Equal(t, "user", byName["inside"].View.Name)
}

func TestRepositoryOperationIsNotAUserOperation(t *testing.T) {
	assert.NotEmpty(t, model.InvalidUserOperation("repos/get"))
	assert.NotEmpty(t, model.InvalidUserOperation("repos/create-using-template"))

	in := userInputs()
	in.UserAuthorizations = []githubgatev1alpha1.GitHubUserAuthorization{
		userAuthorization(adminNamespace, "user", createSigningKey, "repos/create-using-template"),
	}
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, userRule("r", "user", "repos/create-using-template"))}

	decisions, _ := model.Build(in).PolicyRules(policyName(tenantNamespace))
	assert.Equal(t, model.ReasonInvalidSpec, decisions[0].Reason)
}
