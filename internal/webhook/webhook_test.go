package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
)

type fakeInstallations map[types.NamespacedName]*githubgatev1alpha1.GitHubInstallation

func (f fakeInstallations) GetInstallation(_ context.Context, name types.NamespacedName) (*githubgatev1alpha1.GitHubInstallation, error) {
	return f[name], nil
}

func ceilingInstallation() *githubgatev1alpha1.GitHubInstallation {
	return &githubgatev1alpha1.GitHubInstallation{
		Spec: githubgatev1alpha1.GitHubInstallationSpec{
			Ceiling: []githubgatev1alpha1.PermissionSet{{
				Repositories: []githubgatev1alpha1.Repository{"app-*"},
				Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "read"},
			}},
		},
	}
}

func review(t *testing.T, kind, namespace string, object any) *admissionv1.AdmissionResponse {
	t.Helper()

	raw, err := json.Marshal(object)
	require.NoError(t, err)

	body, err := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "u1",
			Kind:      metav1.GroupVersionKind{Group: "githubgate.soliddowant.dev", Version: "v1alpha1", Kind: kind},
			Namespace: namespace,
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
	require.NoError(t, err)

	handler := New([]string{"githubgate"}, fakeInstallations{
		{Namespace: "githubgate", Name: "org"}: ceilingInstallation(),
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/validate", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, recorder.Code)

	var out admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
	require.NotNil(t, out.Response)
	assert.Equal(t, types.UID("u1"), out.Response.UID)

	return out.Response
}

func policy(rule githubgatev1alpha1.AccessRule) *githubgatev1alpha1.GitHubAccessPolicy {
	return &githubgatev1alpha1.GitHubAccessPolicy{
		Spec: githubgatev1alpha1.GitHubAccessPolicySpec{Rules: []githubgatev1alpha1.AccessRule{rule}},
	}
}

func rule(installation string, repos string, perms map[string]githubgatev1alpha1.Level) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:            "r",
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: "githubgate", Name: installation},
		Repositories:    []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(repos)},
		Permissions:     perms,
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}

func userAuthorization(operations ...githubgatev1alpha1.OperationName) *githubgatev1alpha1.GitHubUserAuthorization {
	return &githubgatev1alpha1.GitHubUserAuthorization{
		Spec: githubgatev1alpha1.GitHubUserAuthorizationSpec{
			AppRef:  githubgatev1alpha1.ObjectReference{Name: "app"},
			Login:   "octocat",
			Ceiling: githubgatev1alpha1.UserCeiling{Operations: operations},
		},
	}
}

func userRule(operations ...githubgatev1alpha1.OperationPattern) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:                 "signing-keys",
		UserAuthorizationRef: &githubgatev1alpha1.ObjectReference{Namespace: "githubgate", Name: "user"},
		Proxy:                &githubgatev1alpha1.ProxyMode{Enabled: true, AllowOperations: operations},
	}
}

func TestValidate(t *testing.T) {
	tests := map[string]struct {
		kind      string
		namespace string
		object    any
		allowed   bool
		field     string
	}{
		"app in admin namespace": {
			kind: "GitHubApp", namespace: "githubgate", object: &githubgatev1alpha1.GitHubApp{}, allowed: true,
		},
		"app outside admin namespaces": {
			kind: "GitHubApp", namespace: "tenant", object: &githubgatev1alpha1.GitHubApp{}, field: "metadata.namespace",
		},
		"installation outside admin namespaces": {
			kind: "GitHubInstallation", namespace: "tenant", object: ceilingInstallation(), field: "metadata.namespace",
		},
		"installation with unknown permission": {
			kind: "GitHubInstallation", namespace: "githubgate",
			object: &githubgatev1alpha1.GitHubInstallation{Spec: githubgatev1alpha1.GitHubInstallationSpec{
				Ceiling: []githubgatev1alpha1.PermissionSet{{
					Repositories: []githubgatev1alpha1.Repository{"*"},
					Permissions:  map[string]githubgatev1alpha1.Level{"bogus": "read"},
				}},
			}},
			field: "spec.ceiling[0].permissions[bogus]",
		},
		"installation with organization permission": {
			kind: "GitHubInstallation", namespace: "githubgate",
			object: &githubgatev1alpha1.GitHubInstallation{Spec: githubgatev1alpha1.GitHubInstallationSpec{
				Ceiling: []githubgatev1alpha1.PermissionSet{{
					Repositories: []githubgatev1alpha1.Repository{"*"},
					Permissions:  map[string]githubgatev1alpha1.Level{"members": "read"},
				}},
			}},
			field: "spec.ceiling[0].permissions[members]",
		},
		"installation with valid ceiling": {
			kind: "GitHubInstallation", namespace: "githubgate", object: ceilingInstallation(), allowed: true,
		},
		"policy rule with unknown permission": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object: policy(rule("org", "app-1", map[string]githubgatev1alpha1.Level{"bogus": "read"})),
			field:  "spec.rules[0].permissions[bogus]",
		},
		"policy rule with unknown level": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object: policy(rule("org", "app-1", map[string]githubgatev1alpha1.Level{"contents": "owner"})),
			field:  "spec.rules[0].permissions[contents]",
		},
		"policy rule at the ceiling": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object:  policy(rule("org", "app-1", map[string]githubgatev1alpha1.Level{"contents": "write"})),
			allowed: true,
		},
		"policy rule above the ceiling level": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object: policy(rule("org", "app-1", map[string]githubgatev1alpha1.Level{"issues": "write"})),
			field:  "spec.rules[0]",
		},
		"policy rule outside ceiling repositories": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object: policy(rule("org", "other", map[string]githubgatev1alpha1.Level{"contents": "read"})),
			field:  "spec.rules[0]",
		},
		"user authorization in admin namespace": {
			kind: "GitHubUserAuthorization", namespace: "githubgate",
			object:  userAuthorization("users/create-ssh-signing-key-for-authenticated-user", "users/delete-ssh-signing-key-for-authenticated-user"),
			allowed: true,
		},
		"user authorization outside admin namespaces": {
			kind: "GitHubUserAuthorization", namespace: "tenant",
			object: userAuthorization("users/create-ssh-signing-key-for-authenticated-user"),
			field:  "metadata.namespace",
		},
		"user authorization with an unknown operation": {
			kind: "GitHubUserAuthorization", namespace: "githubgate",
			object: userAuthorization("users/create-ssh-signing-key-for-authenticated-user", "users/no-such-operation"),
			field:  "spec.ceiling.operations[1]",
		},
		"user authorization with a glob operation": {
			kind: "GitHubUserAuthorization", namespace: "githubgate",
			object: userAuthorization("users/*"),
			field:  "spec.ceiling.operations[0]",
		},
		"user authorization with a repository operation": {
			kind: "GitHubUserAuthorization", namespace: "githubgate",
			object: userAuthorization("repos/get"),
			field:  "spec.ceiling.operations[0]",
		},
		"user authorization with an organization operation": {
			kind: "GitHubUserAuthorization", namespace: "githubgate",
			object: userAuthorization("orgs/get"),
			field:  "spec.ceiling.operations[0]",
		},
		"user rule with a user operation": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object:  policy(userRule("users/create-ssh-signing-key-for-authenticated-user")),
			allowed: true,
		},
		"user rule with a repository operation": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object: policy(userRule("repos/get")),
			field:  "spec.rules[0].proxy.allowOperations[0]",
		},
		"policy rule naming a missing installation": {
			kind: "GitHubAccessPolicy", namespace: "tenant",
			object:  policy(rule("missing", "x", map[string]githubgatev1alpha1.Level{"contents": "write"})),
			allowed: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			response := review(t, test.kind, test.namespace, test.object)
			assert.Equal(t, test.allowed, response.Allowed)

			if !test.allowed {
				require.NotNil(t, response.Result)
				assert.Contains(t, response.Result.Message, test.field)
			}
		})
	}
}

func TestMalformedReviewIsRejected(t *testing.T) {
	recorder := httptest.NewRecorder()
	New(nil, fakeInstallations{}).ServeHTTP(recorder,
		httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/validate", bytes.NewReader([]byte("{"))))
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}

func proxyPolicy(proxy githubgatev1alpha1.ProxyMode) *githubgatev1alpha1.GitHubAccessPolicy {
	accessRule := rule("org", "app-1", map[string]githubgatev1alpha1.Level{"contents": "write"})
	accessRule.Token = nil
	proxy.Enabled = true
	accessRule.Proxy = &proxy

	return policy(accessRule)
}

func TestProxyListEntriesMustMatchTheCatalogue(t *testing.T) {
	tests := []struct {
		name    string
		proxy   githubgatev1alpha1.ProxyMode
		allowed bool
		message string
	}{
		{name: "proxy rule without lists", allowed: true},
		{
			name: "entries that match the catalogues",
			proxy: githubgatev1alpha1.ProxyMode{
				PushRefs:        []githubgatev1alpha1.RefPattern{"refs/heads/agent/**"},
				AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/*", "issues/create-comment"},
				DenyOperations:  []githubgatev1alpha1.OperationPattern{"repos/create-release"},
				AllowMutations:  []githubgatev1alpha1.MutationPattern{"create*"},
				DenyMutations:   []githubgatev1alpha1.MutationPattern{"mergePullRequest"},
			},
			allowed: true,
		},
		{
			name:    "an allowed operation that matches nothing",
			proxy:   githubgatev1alpha1.ProxyMode{AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/get", "repos/no-such-thing"}},
			message: `spec.rules[0].proxy.allowOperations[1]: "repos/no-such-thing"`,
		},
		{
			name:    "a denied operation that matches nothing",
			proxy:   githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/create-releases"}},
			message: `spec.rules[0].proxy.denyOperations[0]: "repos/create-releases"`,
		},
		{
			name:    "an operation pattern whose star would need an empty match",
			proxy:   githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/create-release*"}},
			message: `spec.rules[0].proxy.denyOperations[0]: "repos/create-release*"`,
		},
		{
			name:    "an allowed mutation that matches nothing",
			proxy:   githubgatev1alpha1.ProxyMode{AllowMutations: []githubgatev1alpha1.MutationPattern{"createPullRequests"}},
			message: `spec.rules[0].proxy.allowMutations[0]: "createPullRequests"`,
		},
		{
			name:    "a denied mutation that matches nothing",
			proxy:   githubgatev1alpha1.ProxyMode{DenyMutations: []githubgatev1alpha1.MutationPattern{"MergePullRequest"}},
			message: `spec.rules[0].proxy.denyMutations[0]: "MergePullRequest"`,
		},
		{
			name:    "a ref pattern with two dots",
			proxy:   githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/main", "refs/heads/a..b"}},
			message: `spec.rules[0].proxy.pushRefs[1]: "refs/heads/a..b"`,
		},
		{
			name:    "a ref pattern with a reflog selector",
			proxy:   githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/a@{1}"}},
			message: `spec.rules[0].proxy.pushRefs[0]: "refs/heads/a@{1}"`,
		},
		{
			name:    "a malformed ref pattern",
			proxy:   githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/***"}},
			message: `spec.rules[0].proxy.pushRefs[0]: "refs/heads/***"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := review(t, "GitHubAccessPolicy", "tenant", proxyPolicy(test.proxy))
			assert.Equal(t, test.allowed, response.Allowed)

			if !test.allowed {
				require.NotNil(t, response.Result)
				assert.Contains(t, response.Result.Message, test.message)
			}
		})
	}
}
