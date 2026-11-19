package model_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"sync"
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
	adminNamespace  = "githubgate"
	tenantNamespace = "ci-a"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
})

func pkcs1PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})
}

func pkcs8PEM(t *testing.T) []byte {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func ecPEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func int64Ptr(value int64) *int64 { return &value }

func keySecret(namespace string, data map[string][]byte) corev1.Secret {
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "app-key", ResourceVersion: "7"},
		Data:       data,
	}
}

func app(namespace, name string) githubgatev1alpha1.GitHubApp {
	return githubgatev1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: githubgatev1alpha1.GitHubAppSpec{
			AppID:               int64Ptr(42),
			PrivateKeySecretRef: githubgatev1alpha1.SecretKeyReference{Name: "app-key"},
		},
	}
}

func installation(namespace, name string, ceiling ...githubgatev1alpha1.PermissionSet) githubgatev1alpha1.GitHubInstallation {
	return githubgatev1alpha1.GitHubInstallation{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: githubgatev1alpha1.GitHubInstallationSpec{
			AppRef:            githubgatev1alpha1.ObjectReference{Name: "app"},
			Owner:             "solidDoWant",
			Ceiling:           ceiling,
			AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
		},
	}
}

func permissionSet(level githubgatev1alpha1.Level, repositories ...githubgatev1alpha1.Repository) githubgatev1alpha1.PermissionSet {
	return githubgatev1alpha1.PermissionSet{
		Repositories: repositories,
		Permissions:  map[string]githubgatev1alpha1.Level{"contents": level},
	}
}

func rule(name, installationName string, permissions map[string]githubgatev1alpha1.Level, repositories ...githubgatev1alpha1.Repository) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: installationName},
		Repositories:    repositories,
		Permissions:     permissions,
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}

func contents(level githubgatev1alpha1.Level) map[string]githubgatev1alpha1.Level {
	return map[string]githubgatev1alpha1.Level{"contents": level}
}

func policy(namespace string, rules ...githubgatev1alpha1.AccessRule) githubgatev1alpha1.GitHubAccessPolicy {
	return githubgatev1alpha1.GitHubAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "policy"},
		Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
			ServiceAccounts: []string{"ci-build"},
			Rules:           rules,
		},
	}
}

func namespace(name string, labels map[string]string) corev1.Namespace {
	return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// baseInputs is an admin namespace with a ready App and an installation
// whose ceiling gives contents write on foo and bar-*, admitting ci-a.
func baseInputs() model.Inputs {
	return model.Inputs{
		AdminNamespaces: []string{adminNamespace},
		Apps:            []githubgatev1alpha1.GitHubApp{app(adminNamespace, "app")},
		Installations: []githubgatev1alpha1.GitHubInstallation{
			installation(adminNamespace, "main", permissionSet("write", "foo", "bar-*")),
		},
		Secrets: []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"private-key.pem": pkcs1PEM()})},
		Namespaces: []corev1.Namespace{
			namespace(adminNamespace, nil),
			namespace(tenantNamespace, map[string]string{"team": "a"}),
			namespace("ci-b", map[string]string{"team": "b"}),
		},
	}
}

func policyName(namespace string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: "policy"}
}

func TestRuleAcceptance(t *testing.T) {
	tests := []struct {
		name            string
		modify          func(*model.Inputs)
		policyNamespace string
		rule            githubgatev1alpha1.AccessRule
		wantAccepted    bool
		wantReason      string
	}{
		{
			name:         "a rule inside the ceiling is accepted",
			rule:         rule("r", "main", contents("read"), "foo"),
			wantAccepted: true,
			wantReason:   model.ReasonAccepted,
		},
		{
			name:         "a rule at the ceiling's level on a repository its glob covers is accepted",
			rule:         rule("r", "main", contents("write"), "bar-baz", "bar-*"),
			wantAccepted: true,
			wantReason:   model.ReasonAccepted,
		},
		{
			name:       "a repository outside the ceiling is outside the ceiling",
			rule:       rule("r", "main", contents("read"), "foo", "other"),
			wantReason: model.ReasonOutsideCeiling,
		},
		{
			name:       "a permission the ceiling doesn't give is outside the ceiling",
			rule:       rule("r", "main", map[string]githubgatev1alpha1.Level{"contents": "read", "issues": "read"}, "foo"),
			wantReason: model.ReasonOutsideCeiling,
		},
		{
			name: "a permission level above the ceiling's is outside the ceiling",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.Ceiling = []githubgatev1alpha1.PermissionSet{permissionSet("read", "foo")}
			},
			rule:       rule("r", "main", contents("write"), "foo"),
			wantReason: model.ReasonOutsideCeiling,
		},
		{
			name:       "a glob wider than the ceiling's glob is outside the ceiling",
			rule:       rule("r", "main", contents("read"), "ba*"),
			wantReason: model.ReasonOutsideCeiling,
		},
		{
			name:       "an unknown permission is an invalid spec",
			rule:       rule("r", "main", map[string]githubgatev1alpha1.Level{"no_such_permission": "read"}, "foo"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "an organization permission is an invalid spec",
			rule:       rule("r", "main", map[string]githubgatev1alpha1.Level{"members": "read"}, "foo"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "a level the permission doesn't offer is an invalid spec",
			rule:       rule("r", "main", contents("admin"), "foo"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name: "a rule with token mode off is an invalid spec",
			rule: func() githubgatev1alpha1.AccessRule {
				r := rule("r", "main", contents("read"), "foo")
				r.Token = &githubgatev1alpha1.TokenMode{}

				return r
			}(),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "an invalid spec wins over a missing installation",
			rule:       rule("r", "missing", contents("admin"), "foo"),
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "a missing installation is not found",
			rule:       rule("r", "missing", contents("read"), "foo"),
			wantReason: model.ReasonRefNotFound,
		},
		{
			name: "an installation outside the admin namespaces is not found",
			modify: func(in *model.Inputs) {
				in.Apps = append(in.Apps, app(tenantNamespace, "app"))
				in.Installations = append(in.Installations, installation(tenantNamespace, "own", permissionSet("write", "*")))
			},
			rule: func() githubgatev1alpha1.AccessRule {
				r := rule("r", "own", contents("read"), "foo")
				r.InstallationRef.Namespace = ""

				return r
			}(),
			wantReason: model.ReasonRefNotFound,
		},
		{
			name:            "an installation that doesn't admit the policy's namespace is not allowed",
			policyNamespace: "ci-b",
			rule:            rule("r", "main", contents("read"), "foo"),
			wantReason:      model.ReasonNotAllowed,
		},
		{
			name:            "not allowed wins over outside the ceiling",
			policyNamespace: "ci-b",
			rule:            rule("r", "main", contents("read"), "other"),
			wantReason:      model.ReasonNotAllowed,
		},
		{
			name: "an installation admits a namespace its selector matches",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.AllowedNamespaces = githubgatev1alpha1.AllowedNamespaces{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
				}
			},
			policyNamespace: "ci-b",
			rule:            rule("r", "main", contents("read"), "foo"),
			wantAccepted:    true,
			wantReason:      model.ReasonAccepted,
		},
		{
			name: "an installation's selector doesn't admit a namespace whose labels don't match",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.AllowedNamespaces = githubgatev1alpha1.AllowedNamespaces{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
				}
			},
			rule:       rule("r", "main", contents("read"), "foo"),
			wantReason: model.ReasonNotAllowed,
		},
		{
			name: "an installation's selector doesn't admit a namespace that doesn't exist",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.AllowedNamespaces = githubgatev1alpha1.AllowedNamespaces{
					Selector: &metav1.LabelSelector{},
				}
			},
			policyNamespace: "ci-gone",
			rule:            rule("r", "main", contents("read"), "foo"),
			wantReason:      model.ReasonNotAllowed,
		},
		{
			name:            "an installation always admits its own namespace",
			policyNamespace: adminNamespace,
			rule:            rule("r", "main", contents("read"), "foo"),
			wantAccepted:    true,
			wantReason:      model.ReasonAccepted,
		},
		{
			name: "a rule through an installation that isn't ready is accepted but not ready",
			modify: func(in *model.Inputs) {
				in.Secrets = nil
			},
			rule:         rule("r", "main", contents("read"), "foo"),
			wantAccepted: true,
			wantReason:   model.ReasonRefNotReady,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := baseInputs()
			if test.modify != nil {
				test.modify(&in)
			}

			namespace := test.policyNamespace
			if namespace == "" {
				namespace = tenantNamespace
			}

			in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(namespace, test.rule)}

			snapshot := model.Build(in)

			decisions, ok := snapshot.PolicyRules(policyName(namespace))
			require.True(t, ok)
			require.Len(t, decisions, 1)
			assert.Equal(t, test.wantAccepted, decisions[0].Accepted)
			assert.Equal(t, test.wantReason, decisions[0].Reason)
			assert.NotEmpty(t, decisions[0].Message)

			rules := snapshot.Rules(namespace, "ci-build")
			if test.wantAccepted {
				require.Len(t, rules, 1)
				assert.Equal(t, "r", rules[0].Name)
			} else {
				assert.Empty(t, rules)
			}
		})
	}
}

func TestRuleOutsideCeilingLeavesOtherRulesInForce(t *testing.T) {
	in := baseInputs()
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace,
		rule("inside", "main", contents("write"), "foo"),
		rule("outside", "main", contents("read"), "other"),
	)}

	snapshot := model.Build(in)

	decisions, ok := snapshot.PolicyRules(policyName(tenantNamespace))
	require.True(t, ok)
	require.Len(t, decisions, 2)
	assert.True(t, decisions[0].Accepted)
	assert.False(t, decisions[1].Accepted)
	assert.Equal(t, model.ReasonOutsideCeiling, decisions[1].Reason)

	rules := snapshot.Rules(tenantNamespace, "ci-build")
	require.Len(t, rules, 1)
	assert.Equal(t, "inside", rules[0].Name)
	assert.Equal(t, []string{"foo"}, rules[0].Repositories)
	assert.Equal(t, adminNamespace, rules[0].Installation.Namespace)
	assert.Equal(t, "main", rules[0].Installation.Name)
	assert.True(t, rules[0].Installation.Ready)
}

func TestRulesAreIndexedByServiceAccountInThePolicyNamespace(t *testing.T) {
	in := baseInputs()
	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{policy(tenantNamespace, rule("r", "main", contents("read"), "foo"))}

	snapshot := model.Build(in)

	assert.Len(t, snapshot.Rules(tenantNamespace, "ci-build"), 1)
	assert.Empty(t, snapshot.Rules(tenantNamespace, "ci-release"))
	assert.Empty(t, snapshot.Rules("ci-b", "ci-build"))
}

func TestAppsAndInstallationsOutsideAdminNamespacesAreIgnored(t *testing.T) {
	in := baseInputs()
	in.Apps = append(in.Apps, app(tenantNamespace, "tenant-app"))
	in.Secrets = append(in.Secrets, keySecret(tenantNamespace, map[string][]byte{"private-key.pem": pkcs1PEM()}))
	in.Installations = append(in.Installations, installation(tenantNamespace, "tenant", permissionSet("write", "*")))

	viaTenantApp := installation(adminNamespace, "via-tenant-app", permissionSet("write", "*"))
	viaTenantApp.Spec.AppRef = githubgatev1alpha1.ObjectReference{Namespace: tenantNamespace, Name: "tenant-app"}
	in.Installations = append(in.Installations, viaTenantApp)

	snapshot := model.Build(in)

	_, ok := snapshot.App(types.NamespacedName{Namespace: tenantNamespace, Name: "tenant-app"})
	assert.False(t, ok)

	_, ok = snapshot.Installation(types.NamespacedName{Namespace: tenantNamespace, Name: "tenant"})
	assert.False(t, ok)

	for _, installation := range snapshot.Installations() {
		assert.Equal(t, adminNamespace, installation.Namespace)
	}

	through, ok := snapshot.Installation(types.NamespacedName{Namespace: adminNamespace, Name: "via-tenant-app"})
	require.True(t, ok)
	assert.False(t, through.Ready)
	assert.Equal(t, model.ReasonRefNotFound, through.Reason)
	assert.Nil(t, through.App)
}

func TestAppReadiness(t *testing.T) {
	tests := []struct {
		name       string
		modify     func(*githubgatev1alpha1.GitHubApp)
		secrets    []corev1.Secret
		wantReady  bool
		wantReason string
		wantAppID  int64
	}{
		{
			name:      "inline App ID and PKCS#1 key",
			secrets:   []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"private-key.pem": pkcs1PEM()})},
			wantReady: true, wantReason: model.ReasonReady, wantAppID: 42,
		},
		{
			name:      "PKCS#8 key under a custom key name",
			modify:    func(a *githubgatev1alpha1.GitHubApp) { a.Spec.PrivateKeySecretRef.Key = "pem" },
			secrets:   []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"pem": pkcs8PEM(t)})},
			wantReady: true, wantReason: model.ReasonReady, wantAppID: 42,
		},
		{
			name: "App ID from a Secret, whitespace trimmed",
			modify: func(a *githubgatev1alpha1.GitHubApp) {
				a.Spec.AppID = nil
				a.Spec.AppIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-key"}
			},
			secrets: []corev1.Secret{keySecret(adminNamespace, map[string][]byte{
				"private-key.pem": pkcs1PEM(), "app-id": []byte(" 1234\n"),
			})},
			wantReady: true, wantReason: model.ReasonReady, wantAppID: 1234,
		},
		{
			name: "App ID in a Secret that isn't a number",
			modify: func(a *githubgatev1alpha1.GitHubApp) {
				a.Spec.AppID = nil
				a.Spec.AppIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-key"}
			},
			secrets: []corev1.Secret{keySecret(adminNamespace, map[string][]byte{
				"private-key.pem": pkcs1PEM(), "app-id": []byte("abc"),
			})},
			wantReason: model.ReasonSecretInvalid,
		},
		{
			name:       "missing key Secret",
			wantReason: model.ReasonSecretUnavailable,
		},
		{
			name:       "key Secret in another namespace is not used",
			secrets:    []corev1.Secret{keySecret(tenantNamespace, map[string][]byte{"private-key.pem": pkcs1PEM()})},
			wantReason: model.ReasonSecretUnavailable,
		},
		{
			name:       "key Secret without the key",
			secrets:    []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"other": pkcs1PEM()})},
			wantReason: model.ReasonSecretUnavailable,
		},
		{
			name:       "key that isn't PEM",
			secrets:    []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"private-key.pem": []byte("not a key")})},
			wantReason: model.ReasonSecretInvalid,
		},
		{
			name:       "key that isn't RSA",
			secrets:    []corev1.Secret{keySecret(adminNamespace, map[string][]byte{"private-key.pem": ecPEM(t)})},
			wantReason: model.ReasonSecretInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := app(adminNamespace, "app")
			if test.modify != nil {
				test.modify(&object)
			}

			snapshot := model.Build(model.Inputs{
				AdminNamespaces: []string{adminNamespace},
				Apps:            []githubgatev1alpha1.GitHubApp{object},
				Secrets:         test.secrets,
			})

			got, ok := snapshot.App(types.NamespacedName{Namespace: adminNamespace, Name: "app"})
			require.True(t, ok)
			assert.Equal(t, test.wantReady, got.Ready)
			assert.Equal(t, test.wantReason, got.Reason)

			if test.wantReady {
				assert.Equal(t, test.wantAppID, got.AppID)
				require.NotNil(t, got.PrivateKey())
				assert.True(t, got.PrivateKey().Equal(testKey()))
				assert.NotEmpty(t, got.KeyVersion)
			} else {
				assert.Nil(t, got.PrivateKey())
				assert.NotContains(t, got.Message, "PRIVATE KEY")
			}
		})
	}
}

func TestInstallationReadiness(t *testing.T) {
	tests := []struct {
		name       string
		modify     func(*model.Inputs)
		wantReady  bool
		wantReason string
	}{
		{
			name:       "ready App and valid ceiling",
			wantReady:  true,
			wantReason: model.ReasonReady,
		},
		{
			name: "ceiling with an unknown permission",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.Ceiling[0].Permissions = map[string]githubgatev1alpha1.Level{"no_such_permission": "read"}
			},
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name: "ceiling with an organization permission",
			modify: func(in *model.Inputs) {
				in.Installations[0].Spec.Ceiling[0].Permissions = map[string]githubgatev1alpha1.Level{"members": "read"}
			},
			wantReason: model.ReasonInvalidSpec,
		},
		{
			name:       "missing App",
			modify:     func(in *model.Inputs) { in.Apps = nil },
			wantReason: model.ReasonRefNotFound,
		},
		{
			name: "App that doesn't admit the installation's namespace",
			modify: func(in *model.Inputs) {
				in.AdminNamespaces = append(in.AdminNamespaces, "other-admin")
				in.Installations[0].Namespace = "other-admin"
				in.Installations[0].Spec.AppRef.Namespace = adminNamespace
			},
			wantReason: model.ReasonNotAllowed,
		},
		{
			name:       "App that isn't ready",
			modify:     func(in *model.Inputs) { in.Secrets = nil },
			wantReason: model.ReasonRefNotReady,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := baseInputs()
			if test.modify != nil {
				test.modify(&in)
			}

			snapshot := model.Build(in)

			got, ok := snapshot.Installation(types.NamespacedName{Namespace: in.Installations[0].Namespace, Name: "main"})
			require.True(t, ok)
			assert.Equal(t, test.wantReady, got.Ready)
			assert.Equal(t, test.wantReason, got.Reason)
		})
	}
}

func TestStoreIsNotReadyUntilTheFirstSnapshot(t *testing.T) {
	var store model.Store
	assert.False(t, store.Ready())
	assert.Nil(t, store.Load())

	snapshot := model.Build(baseInputs())
	store.Swap(snapshot)
	assert.True(t, store.Ready())
	assert.Same(t, snapshot, store.Load())
}

func TestCacheOptionsReadSecretsOnlyInAdminNamespaces(t *testing.T) {
	options := model.CacheOptions([]string{adminNamespace, "other-admin"})

	require.Len(t, options.ByObject, 1)

	for object, byObject := range options.ByObject {
		assert.IsType(t, &corev1.Secret{}, object)
		assert.Len(t, byObject.Namespaces, 2)
		assert.Contains(t, byObject.Namespaces, adminNamespace)
		assert.Contains(t, byObject.Namespaces, "other-admin")
	}

	assert.Empty(t, options.DefaultNamespaces)
}
