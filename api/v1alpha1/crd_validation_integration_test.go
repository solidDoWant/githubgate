//go:build integration

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// proxyPolicy returns a valid policy whose rule enables only proxy mode.
func proxyPolicy(name string, proxy githubgatev1alpha1.ProxyMode) *githubgatev1alpha1.GitHubAccessPolicy {
	policy := validPolicy(name)
	proxy.Enabled = true
	policy.Spec.Rules[0].Token = nil
	policy.Spec.Rules[0].Proxy = &proxy

	return policy
}

// tokenProxyPolicy returns a policy whose rule enables token mode and has
// the given proxy mode.
func tokenProxyPolicy(name string, proxy githubgatev1alpha1.ProxyMode) *githubgatev1alpha1.GitHubAccessPolicy {
	policy := proxyPolicy(name, proxy)
	policy.Spec.Rules[0].Token = &githubgatev1alpha1.TokenMode{Enabled: true}

	return policy
}

// emptyProxyListPolicy returns a proxy-only policy whose rule sets one proxy
// list to an empty list. It is unstructured because the typed object omits
// empty lists.
func emptyProxyListPolicy(name, field string) client.Object {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(proxyPolicy(name, githubgatev1alpha1.ProxyMode{}))
	if err != nil {
		panic(err)
	}

	rules, _, _ := unstructured.NestedSlice(content, "spec", "rules")
	rule := rules[0].(map[string]any)
	rule["proxy"].(map[string]any)[field] = []any{}

	if err := unstructured.SetNestedSlice(content, rules, "spec", "rules"); err != nil {
		panic(err)
	}

	object := &unstructured.Unstructured{Object: content}
	object.SetGroupVersionKind(githubgatev1alpha1.GroupVersion.WithKind("GitHubAccessPolicy"))

	return object
}

// policyWithLists returns a valid policy with the given number of distinct
// ServiceAccounts, rules and repositories in each rule.
func policyWithLists(name string, serviceAccounts, rules, repositories int) *githubgatev1alpha1.GitHubAccessPolicy {
	policy := validPolicy(name)

	policy.Spec.ServiceAccounts = nil
	for index := range serviceAccounts {
		policy.Spec.ServiceAccounts = append(policy.Spec.ServiceAccounts, "sa-"+strconv.Itoa(index))
	}

	patterns := []githubgatev1alpha1.Repository{}
	for index := range repositories {
		patterns = append(patterns, githubgatev1alpha1.Repository("repo-"+strconv.Itoa(index)))
	}

	template := policy.Spec.Rules[0]
	policy.Spec.Rules = nil

	for index := range rules {
		rule := template
		rule.Name = "rule-" + strconv.Itoa(index)
		rule.Repositories = patterns
		policy.Spec.Rules = append(policy.Spec.Rules, rule)
	}

	return policy
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
		{name: "policy rule with only proxy mode", object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{})
		}},
		{name: "policy rule with both modes", object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Proxy = &githubgatev1alpha1.ProxyMode{Enabled: true}

			return policy
		}},
		{name: "policy rule with proxy disabled and no token", invalid: true, object: func(n string) client.Object {
			policy := proxyPolicy(n, githubgatev1alpha1.ProxyMode{})
			policy.Spec.Rules[0].Proxy.Enabled = false

			return policy
		}},
		{name: "policy rule with token and proxy both disabled", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules[0].Token.Enabled = false
			policy.Spec.Rules[0].Proxy = &githubgatev1alpha1.ProxyMode{}

			return policy
		}},
		{name: "proxy rule with every list", object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{
				PushRefs:        []githubgatev1alpha1.RefPattern{"refs/heads/agent/**"},
				AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/*"},
				DenyOperations:  []githubgatev1alpha1.OperationPattern{"repos/create-release"},
				AllowMutations:  []githubgatev1alpha1.MutationPattern{"create*"},
				DenyMutations:   []githubgatev1alpha1.MutationPattern{"mergePullRequest"},
			})
		}},
		{name: "token rule with proxy pushRefs", invalid: true, object: func(n string) client.Object {
			return tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/x"}})
		}},
		{name: "token rule with proxy allowOperations", invalid: true, object: func(n string) client.Object {
			return tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{AllowOperations: []githubgatev1alpha1.OperationPattern{"repos/get"}})
		}},
		{name: "token rule with proxy denyOperations", invalid: true, object: func(n string) client.Object {
			return tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/get"}})
		}},
		{name: "token rule with proxy allowMutations", invalid: true, object: func(n string) client.Object {
			return tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{AllowMutations: []githubgatev1alpha1.MutationPattern{"addStar"}})
		}},
		{name: "token rule with proxy denyMutations", invalid: true, object: func(n string) client.Object {
			return tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{DenyMutations: []githubgatev1alpha1.MutationPattern{"addStar"}})
		}},
		{name: "token rule with disabled proxy carrying a list", invalid: true, object: func(n string) client.Object {
			policy := tokenProxyPolicy(n, githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/get"}})
			policy.Spec.Rules[0].Proxy.Enabled = false

			return policy
		}},
		{name: "proxy rule with public read and web paths", object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{PublicRead: true, WebPaths: true})
		}},
		{name: "proxy rule with public read and only repository patterns", invalid: true, object: func(n string) client.Object {
			policy := proxyPolicy(n, githubgatev1alpha1.ProxyMode{PublicRead: true})
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{"app-*"}

			return policy
		}},
		{name: "proxy rule with web paths and only repository patterns", object: func(n string) client.Object {
			policy := proxyPolicy(n, githubgatev1alpha1.ProxyMode{WebPaths: true})
			policy.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{"app-*"}

			return policy
		}},
		{name: "proxy rule with empty pushRefs", invalid: true, object: func(n string) client.Object {
			return emptyProxyListPolicy(n, "pushRefs")
		}},
		{name: "proxy rule with empty denyOperations", invalid: true, object: func(n string) client.Object {
			return emptyProxyListPolicy(n, "denyOperations")
		}},
		{name: "proxy rule with empty allowMutations", invalid: true, object: func(n string) client.Object {
			return emptyProxyListPolicy(n, "allowMutations")
		}},
		{name: "proxy rule with a ref outside refs", invalid: true, object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"heads/main"}})
		}},
		{name: "proxy rule with a ref containing a backslash", invalid: true, object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/a\\b"}})
		}},
		{name: "proxy rule with a ref containing a control character", invalid: true, object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/a\x01"}})
		}},
		{name: "proxy rule with an upper-case operation", invalid: true, object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{DenyOperations: []githubgatev1alpha1.OperationPattern{"Repos/get"}})
		}},
		{name: "proxy rule with a mutation containing a slash", invalid: true, object: func(n string) client.Object {
			return proxyPolicy(n, githubgatev1alpha1.ProxyMode{DenyMutations: []githubgatev1alpha1.MutationPattern{"merge/x"}})
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
		{name: "policy with 256 service accounts", object: func(n string) client.Object {
			return policyWithLists(n, 256, 1, 1)
		}},
		{name: "policy with 257 service accounts", invalid: true, object: func(n string) client.Object {
			return policyWithLists(n, 257, 1, 1)
		}},
		{name: "policy with 256 rules", object: func(n string) client.Object {
			return policyWithLists(n, 1, 256, 1)
		}},
		{name: "policy with 257 rules", invalid: true, object: func(n string) client.Object {
			return policyWithLists(n, 1, 257, 1)
		}},
		{name: "policy rule with 256 repositories", object: func(n string) client.Object {
			return policyWithLists(n, 1, 1, 256)
		}},
		{name: "policy rule with 257 repositories", invalid: true, object: func(n string) client.Object {
			return policyWithLists(n, 1, 1, 257)
		}},
		{name: "policy with duplicate rule names", invalid: true, object: func(n string) client.Object {
			policy := validPolicy(n)
			policy.Spec.Rules = append(policy.Spec.Rules, policy.Spec.Rules[0])

			return policy
		}},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := test.object(strings.ReplaceAll(strings.ToLower(test.name), " ", "-") + "-" + strconv.Itoa(index))
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
