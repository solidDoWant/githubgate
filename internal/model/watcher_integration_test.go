//go:build integration

package model_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

type watcherEnv struct {
	client client.Client
	cache  cache.Cache
	store  *model.Store
}

func startWatcher(t *testing.T) watcherEnv {
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

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	for _, name := range []string{adminNamespace, tenantNamespace} {
		require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	options := model.CacheOptions([]string{adminNamespace})
	options.Scheme = scheme
	informers, err := cache.New(cfg, options)
	require.NoError(t, err)

	store := &model.Store{}
	watcher := &model.Watcher{Cache: informers, Store: store, AdminNamespaces: []string{adminNamespace}, Log: logr.Discard()}

	ctx := t.Context()
	cacheDone := make(chan error, 1)
	watcherDone := make(chan error, 1)

	go func() { cacheDone <- informers.Start(ctx) }()
	go func() { watcherDone <- watcher.Start(ctx) }()

	t.Cleanup(func() {
		require.NoError(t, <-watcherDone)
		require.NoError(t, <-cacheDone)
	})

	return watcherEnv{client: c, cache: informers, store: store}
}

func TestWatcherRebuildsTheSnapshotWhenAPolicyChanges(t *testing.T) {
	env := startWatcher(t)
	ctx := t.Context()

	require.Eventually(t, env.store.Ready, 30*time.Second, 50*time.Millisecond)

	keyObject := keySecret(adminNamespace, map[string][]byte{"private-key.pem": pkcs1PEM()})
	keyObject.ResourceVersion = ""
	appObject := app(adminNamespace, "app")
	installationObject := installation(adminNamespace, "main", permissionSet("write", "*"))
	policyObject := policy(tenantNamespace, rule("r", "main", contents("read"), "foo", "bar"))

	for _, object := range []client.Object{&keyObject, &appObject, &installationObject, &policyObject} {
		require.NoError(t, env.client.Create(ctx, object))
	}

	repositories := func() []string {
		rules := env.store.Load().Rules(tenantNamespace, "ci-build")
		if len(rules) != 1 || !rules[0].Installation.Ready {
			return nil
		}

		return rules[0].Repositories
	}

	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"foo", "bar"}, repositories()) },
		10*time.Second, 50*time.Millisecond)

	require.NoError(t, env.client.Get(ctx, types.NamespacedName{Namespace: tenantNamespace, Name: "policy"}, &policyObject))
	policyObject.Spec.Rules[0].Repositories = []githubgatev1alpha1.Repository{"foo"}
	require.NoError(t, env.client.Update(ctx, &policyObject))

	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"foo"}, repositories()) },
		5*time.Second, 50*time.Millisecond)
}

func TestWatcherCacheReadsSecretsOnlyInAdminNamespaces(t *testing.T) {
	env := startWatcher(t)
	ctx := t.Context()

	require.Eventually(t, env.store.Ready, 30*time.Second, 50*time.Millisecond)

	tenantSecret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "tenant"}}
	adminSecret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "admin"}}

	require.NoError(t, env.client.Create(ctx, &tenantSecret))
	require.NoError(t, env.client.Create(ctx, &adminSecret))

	require.Eventually(t, func() bool {
		return env.cache.Get(ctx, client.ObjectKeyFromObject(&adminSecret), &corev1.Secret{}) == nil
	}, 10*time.Second, 50*time.Millisecond)

	require.Error(t, env.cache.Get(ctx, client.ObjectKeyFromObject(&tenantSecret), &corev1.Secret{}))
	require.Error(t, env.cache.List(ctx, &corev1.SecretList{}, client.InNamespace(tenantNamespace)))
}
