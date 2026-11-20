//go:build integration

package server_test

import (
	"net/http"
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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

// TestOneReplicaMintsOnceForRepeatedRequests runs the token API on a snapshot
// the watcher builds from a real API server, so the configuration goes
// through the same caches as in a cluster.
func TestOneReplicaMintsOnceForRepeatedRequests(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}

	f := newFixture(t)
	f.store.Swap(nil)

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

	ctx := t.Context()

	for _, name := range []string{adminNamespace, tenantNamespace} {
		require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	secret := f.inputs.Secrets[0]
	secret.ResourceVersion = ""
	objects := []client.Object{&secret, &f.inputs.Apps[0], &f.inputs.Installations[0], &f.inputs.Policies[0]}

	for _, object := range objects {
		require.NoError(t, c.Create(ctx, object))
	}

	options := model.CacheOptions([]string{adminNamespace})
	options.Scheme = scheme
	informers, err := cache.New(cfg, options)
	require.NoError(t, err)

	watcher := &model.Watcher{Cache: informers, Store: f.store, AdminNamespaces: []string{adminNamespace}, Log: logr.Discard()}
	cacheDone := make(chan error, 1)
	watcherDone := make(chan error, 1)

	go func() { cacheDone <- informers.Start(ctx) }()
	go func() { watcherDone <- watcher.Start(ctx) }()

	t.Cleanup(func() {
		require.NoError(t, <-watcherDone)
		require.NoError(t, <-cacheDone)
	})

	var first response

	require.Eventually(t, func() bool {
		first = f.request(t, caller, `{"repositories": ["foo"]}`)

		return first.status == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond)

	for range 5 {
		again := f.request(t, caller, `{"repositories": ["foo"]}`)
		require.Equal(t, http.StatusOK, again.status, again.err.Message)
		assert.Equal(t, first.token.Token, again.token.Token)
	}

	assert.Len(t, f.fake.MintLog(), 1)

	// Removing the repository from the policy refuses the next request even
	// though a token for it is cached.
	policy := &f.inputs.Policies[0]
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(policy), policy))
	policy.Spec.Rules = policy.Spec.Rules[1:]
	require.NoError(t, c.Update(ctx, policy))

	require.Eventually(t, func() bool {
		return f.request(t, caller, `{"repositories": ["foo"]}`).status == http.StatusForbidden
	}, 10*time.Second, 50*time.Millisecond)
	assert.Len(t, f.fake.MintLog(), 1)
}
