//go:build integration

package controller_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/controller"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	adminNamespace  = "githubgate"
	tenantNamespace = "tenant"
	githubAppID     = 7
	owner           = "acme"
)

// countingTransport counts the requests one replica sends to GitHub.
type countingTransport struct{ requests atomic.Int64 }

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.requests.Add(1)

	return http.DefaultTransport.RoundTrip(request)
}

type env struct {
	client   client.Client
	fake     *fakegithub.Server
	key      *rsa.PrivateKey
	replicas []*countingTransport
}

func keyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func startEnv(t *testing.T) *env {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, githubgatev1alpha1.AddToScheme(scheme))

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	for _, name := range []string{adminNamespace, tenantNamespace} {
		require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	fake, err := fakegithub.New(t.TempDir())
	require.NoError(t, err)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	fake.CreateApp(fakegithub.App{ID: githubAppID, Name: "ci-app", PublicKey: &key.PublicKey})
	require.NoError(t, fake.CreateInstallation(fakegithub.Installation{
		ID: 70, AppID: githubAppID, Owner: owner, Permissions: map[string]string{"contents": "write"},
	}))

	api := httptest.NewServer(fake.Handler())
	t.Cleanup(api.Close)

	result := &env{client: c, fake: fake, key: key}

	for range 3 {
		transport := &countingTransport{}
		result.replicas = append(result.replicas, transport)

		options := model.CacheOptions([]string{adminNamespace})
		options.Scheme = scheme

		manager, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                  scheme,
			Cache:                   options,
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:  "0",
			LeaderElection:          true,
			LeaderElectionID:        "githubgate-status",
			LeaderElectionNamespace: adminNamespace,
			Controller:              config.Controller{SkipNameValidation: ptr.To(true)},
		})
		require.NoError(t, err)

		registry := github.NewRegistry(github.Options{BaseURL: api.URL, Transport: transport})
		require.NoError(t, controller.Setup(manager, controller.Options{
			AdminNamespaces: []string{adminNamespace}, GitHub: registry,
			OAuthURL: api.URL, APIURL: api.URL, Transport: transport,
		}))

		done := make(chan error, 1)

		go func() { done <- manager.Start(t.Context()) }()

		t.Cleanup(func() { require.NoError(t, <-done) })
	}

	return result
}

func (e *env) create(t *testing.T, object client.Object) {
	t.Helper()
	require.NoError(t, e.client.Create(t.Context(), object))
}

func (e *env) keySecret(t *testing.T, name string, data map[string][]byte) {
	t.Helper()
	e.create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name}, Data: data})
}

func app(name, keySecret string) *githubgatev1alpha1.GitHubApp {
	return &githubgatev1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
		Spec: githubgatev1alpha1.GitHubAppSpec{
			AppID:               ptr.To[int64](githubAppID),
			PrivateKeySecretRef: githubgatev1alpha1.SecretKeyReference{Name: keySecret},
		},
	}
}

func installation(name, appName, ownerName string, ceilingLevel githubgatev1alpha1.Level) *githubgatev1alpha1.GitHubInstallation {
	return &githubgatev1alpha1.GitHubInstallation{
		ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
		Spec: githubgatev1alpha1.GitHubInstallationSpec{
			AppRef: githubgatev1alpha1.ObjectReference{Name: appName},
			Owner:  ownerName,
			Ceiling: []githubgatev1alpha1.PermissionSet{{
				Repositories: []githubgatev1alpha1.Repository{"*"},
				Permissions:  map[string]githubgatev1alpha1.Level{"contents": ceilingLevel},
			}},
			AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
		},
	}
}

// readyCondition waits for the Ready condition of an object to have the
// wanted status and reason, and returns the object's conditions.
func (e *env) requireReady(t *testing.T, object client.Object, conditions func() []metav1.Condition, status metav1.ConditionStatus, reason string) {
	t.Helper()

	require.Eventually(t, func() bool {
		if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			return false
		}

		condition := apimeta.FindStatusCondition(conditions(), controller.ConditionReady)

		return condition != nil && condition.Status == status && condition.Reason == reason
	}, 30*time.Second, 100*time.Millisecond, "waiting for Ready=%s reason=%s", status, reason)
}

func TestStatusOfAppsInstallationsAndPolicies(t *testing.T) {
	e := startEnv(t)

	t.Run("an App with a missing or invalid key Secret recovers without a restart", func(t *testing.T) {
		object := app("broken", "broken-key")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "SecretUnavailable")
		assert.Equal(t, object.Generation, object.Status.ObservedGeneration)

		e.keySecret(t, "broken-key", map[string][]byte{"private-key.pem": []byte("not a key")})
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "SecretInvalid")

		var secret corev1.Secret
		require.NoError(t, e.client.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: "broken-key"}, &secret))
		secret.Data["private-key.pem"] = keyPEM(e.key)
		require.NoError(t, e.client.Update(t.Context(), &secret))
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionTrue, "Ready")
		assert.Equal(t, "ci-app", object.Status.Slug)
	})

	e.keySecret(t, "key", map[string][]byte{"private-key.pem": keyPEM(e.key)})
	e.keySecret(t, "app-id", map[string][]byte{"app-id": []byte(strconv.Itoa(githubAppID))})

	t.Run("an App ID inline or from a Secret reaches Ready", func(t *testing.T) {
		inline := app("inline", "key")
		fromSecret := app("from-secret", "key")
		fromSecret.Spec.AppID = nil
		fromSecret.Spec.AppIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "app-id"}

		e.create(t, inline)
		e.create(t, fromSecret)

		e.requireReady(t, inline, func() []metav1.Condition { return inline.Status.Conditions }, metav1.ConditionTrue, "Ready")
		e.requireReady(t, fromSecret, func() []metav1.Condition { return fromSecret.Status.Conditions }, metav1.ConditionTrue, "Ready")
	})

	t.Run("an installation on an owner the App is not installed on is InstallationNotFound", func(t *testing.T) {
		object := installation("elsewhere", "inline", "nobody", "read")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "InstallationNotFound")
	})

	t.Run("a ceiling above the grant sets CeilingExceedsGrant", func(t *testing.T) {
		within := installation("within", "inline", owner, "write")
		above := installation("above", "inline", owner, "write")
		above.Spec.Ceiling[0].Permissions = map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "write"}

		e.create(t, within)
		e.create(t, above)
		e.requireReady(t, within, func() []metav1.Condition { return within.Status.Conditions }, metav1.ConditionTrue, "Ready")
		e.requireReady(t, above, func() []metav1.Condition { return above.Status.Conditions }, metav1.ConditionTrue, "Ready")

		assert.Equal(t, int64(70), above.Status.InstallationID)
		assert.Equal(t, githubgatev1alpha1.Level("write"), above.Status.GrantedPermissions["contents"])
		assert.True(t, apimeta.IsStatusConditionTrue(above.Status.Conditions, controller.ConditionCeilingExceedsGrant))
		assert.True(t, apimeta.IsStatusConditionFalse(within.Status.Conditions, controller.ConditionCeilingExceedsGrant))
	})

	t.Run("a policy reports each rule and Ready=False while a rule is not accepted", func(t *testing.T) {
		policy := &githubgatev1alpha1.GitHubAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "policy"},
			Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
				ServiceAccounts: []string{"ci"},
				Rules: []githubgatev1alpha1.AccessRule{
					rule("ok", "within", "write"),
					rule("too-much", "within", "write"),
					rule("missing", "nonexistent", "read"),
				},
			},
		}
		e.create(t, policy)
		e.requireReady(t, policy, func() []metav1.Condition { return policy.Status.Conditions }, metav1.ConditionFalse, controller.ReasonRulesNotAccepted)

		reasons := map[string]string{}

		for _, ruleStatus := range policy.Status.Rules {
			condition := apimeta.FindStatusCondition(ruleStatus.Conditions, controller.ConditionAccepted)
			require.NotNil(t, condition)
			reasons[ruleStatus.Name] = string(condition.Status) + "/" + condition.Reason
		}

		assert.Equal(t, map[string]string{
			"ok": "True/Accepted", "too-much": "False/OutsideCeiling", "missing": "False/RefNotFound",
		}, reasons)
	})

	t.Run("only the elected leader writes status", func(t *testing.T) {
		busy := 0

		for _, replica := range e.replicas {
			if replica.requests.Load() > 0 {
				busy++
			}
		}

		assert.Equal(t, 1, busy, "exactly one of three replicas should have called GitHub")
	})
}

func rule(name, installationName string, level githubgatev1alpha1.Level) githubgatev1alpha1.AccessRule {
	permission := "contents"
	if name == "too-much" {
		permission = "issues"
	}

	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: installationName},
		Repositories:    []githubgatev1alpha1.Repository{"foo"},
		Permissions:     map[string]githubgatev1alpha1.Level{permission: level},
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}
