//go:build integration

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
)

func newClient(t *testing.T) client.Client {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, githubgatev1alpha1.AddToScheme(scheme))
	_ = apiextensionsv1.AddToScheme(scheme)

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	return c
}

func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "default"}
}

func int64Ptr(value int64) *int64 { return &value }

func validApp(name string) *githubgatev1alpha1.GitHubApp {
	return &githubgatev1alpha1.GitHubApp{
		ObjectMeta: objectMeta(name),
		Spec: githubgatev1alpha1.GitHubAppSpec{
			AppID:               int64Ptr(123),
			PrivateKeySecretRef: githubgatev1alpha1.SecretKeyReference{Name: "key"},
		},
	}
}

func validInstallation(name string) *githubgatev1alpha1.GitHubInstallation {
	return &githubgatev1alpha1.GitHubInstallation{
		ObjectMeta: objectMeta(name),
		Spec: githubgatev1alpha1.GitHubInstallationSpec{
			AppRef: githubgatev1alpha1.ObjectReference{Name: "app"},
			Owner:  "solidDoWant",
			Ceiling: []githubgatev1alpha1.PermissionSet{{
				Repositories: []githubgatev1alpha1.Repository{"*"},
				Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write"},
			}},
		},
	}
}

func validPolicy(name string) *githubgatev1alpha1.GitHubAccessPolicy {
	return &githubgatev1alpha1.GitHubAccessPolicy{
		ObjectMeta: objectMeta(name),
		Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
			ServiceAccounts: []string{"ci-build"},
			Rules: []githubgatev1alpha1.AccessRule{{
				Name:            "read",
				InstallationRef: githubgatev1alpha1.ObjectReference{Name: "inst"},
				Repositories:    []githubgatev1alpha1.Repository{"foo", "ci-*", "a.b_c"},
				Permissions:     map[string]githubgatev1alpha1.Level{"contents": "read"},
				Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
			}},
		},
	}
}

func TestCRDValidation(t *testing.T) {
	k8s := newClient(t)

	tests := []struct {
		name    string
		object  func(name string) client.Object
		invalid bool
	}{
		{name: "app with inline ID", object: func(n string) client.Object { return validApp(n) }},
		{name: "app with secret ID", object: func(n string) client.Object {
			app := validApp(n)
			app.Spec.AppID = nil
			app.Spec.AppIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "id"}

			return app
		}},
		{name: "app with both IDs", invalid: true, object: func(n string) client.Object {
			app := validApp(n)
			app.Spec.AppIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "id"}

			return app
		}},
		{name: "app with neither ID", invalid: true, object: func(n string) client.Object {
			app := validApp(n)
			app.Spec.AppID = nil

			return app
		}},
		{name: "installation", object: func(n string) client.Object { return validInstallation(n) }},
		{name: "installation with empty ceiling", invalid: true, object: func(n string) client.Object {
			inst := validInstallation(n)
			inst.Spec.Ceiling = nil

			return inst
		}},
		{name: "installation with unknown level", invalid: true, object: func(n string) client.Object {
			inst := validInstallation(n)
			inst.Spec.Ceiling[0].Permissions["contents"] = "owner"

			return inst
		}},
		{name: "installation ceiling with slash in repository", invalid: true, object: func(n string) client.Object {
			inst := validInstallation(n)
			inst.Spec.Ceiling[0].Repositories = []githubgatev1alpha1.Repository{"owner/foo"}

			return inst
		}},
		{name: "policy", object: func(n string) client.Object { return validPolicy(n) }},
		{name: "policy rule with token disabled", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Token.Enabled = false

			return policy
		}},
		{name: "policy rule with no mode", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Token = nil

			return policy
		}},
		{name: "policy repository with slash", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{"owner/foo"}

			return policy
		}},
		{name: "policy repository with space", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{"fo o"}

			return policy
		}},
		{name: "policy repository of 100 characters", object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(strings.Repeat("a", 100))}

			return policy
		}},
		{name: "policy repository of 101 characters", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(strings.Repeat("a", 101))}

			return policy
		}},
		{name: "policy rule without repositories", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Repositories = nil

			return policy
		}},
		{name: "policy rule without permissions", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Permissions = map[string]githubgatev1alpha1.Level{}

			return policy
		}},
		{name: "policy without service accounts", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.ServiceAccounts = nil

			return policy
		}},
		{name: "policy with invalid service account name", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.ServiceAccounts = []string{"Not_Valid"}

			return policy
		}},
		{name: "policy with duplicate rule names", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules = append(policy.Spec.Rules, policy.Spec.Rules[0])

			return policy
		}},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := test.object(strings.ReplaceAll(strings.ToLower(test.name), " ", "-") + "-" + string(rune('a'+index)))
			err := k8s.Create(t.Context(), object)

			if test.invalid {
				require.Error(t, err)
				require.True(t, isInvalid(err), "want an Invalid error, got %v", err)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestAllKindsHaveTheGithubgateCategory(t *testing.T) {
	k8s := newClient(t)

	var crds apiextensionsv1.CustomResourceDefinitionList
	require.NoError(t, k8s.List(t.Context(), &crds))

	kinds := map[string]bool{}

	for _, crd := range crds.Items {
		if crd.Spec.Group != githubgatev1alpha1.GroupVersion.Group {
			continue
		}

		for _, category := range crd.Spec.Names.Categories {
			if category == "githubgate" {
				kinds[crd.Spec.Names.Kind] = true
			}
		}
	}

	require.Equal(t, map[string]bool{"GitHubApp": true, "GitHubInstallation": true, "GitHubAccessPolicy": true}, kinds)
}

func isInvalid(err error) bool {
	var status interface{ Status() metav1.Status }
	if e, ok := err.(interface{ Status() metav1.Status }); ok {
		status = e
	}

	return status != nil && status.Status().Reason == metav1.StatusReasonInvalid
}
