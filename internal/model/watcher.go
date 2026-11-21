package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
)

// retryDelay is how long the watcher waits before building again after a
// failed build.
const retryDelay = time.Second

// CacheOptions restricts the cache so that Secrets are read only in the admin
// namespaces; everything else is cached cluster-wide.
func CacheOptions(adminNamespaces []string) cache.Options {
	namespaces := make(map[string]cache.Config, len(adminNamespaces))
	for _, namespace := range adminNamespaces {
		namespaces[namespace] = cache.Config{}
	}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: namespaces},
		},
	}
}

// Watcher rebuilds the snapshot in Store whenever a GitHubApp,
// GitHubInstallation, GitHubAccessPolicy, admin-namespace Secret or Namespace
// changes. It runs on every replica, leader or not.
type Watcher struct {
	Cache           cache.Cache
	Store           *Store
	AdminNamespaces []string
	Log             logr.Logger
}

// NeedLeaderElection reports that every replica builds its own snapshot.
func (w *Watcher) NeedLeaderElection() bool { return false }

// Start watches the cache and rebuilds until ctx ends.
func (w *Watcher) Start(ctx context.Context) error {
	if len(w.AdminNamespaces) == 0 {
		return errors.New("no admin namespaces")
	}

	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { notify() },
		UpdateFunc: func(any, any) { notify() },
		DeleteFunc: func(any) { notify() },
	}

	watched := []client.Object{
		&githubgatev1alpha1.GitHubApp{},
		&githubgatev1alpha1.GitHubInstallation{},
		&githubgatev1alpha1.GitHubAccessPolicy{},
		&githubgatev1alpha1.GitHubUserAuthorization{},
		&corev1.Secret{},
		&corev1.Namespace{},
	}
	for _, object := range watched {
		informer, err := w.Cache.GetInformer(ctx, object)
		if err != nil {
			return fmt.Errorf("watching %T: %w", object, err)
		}

		if _, err := informer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("watching %T: %w", object, err)
		}
	}

	if !w.Cache.WaitForCacheSync(ctx) {
		return ctx.Err()
	}

	notify()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}

		if err := w.rebuild(ctx); err != nil {
			w.Log.Error(err, "building the snapshot failed; keeping the previous one")

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
				notify()
			}
		}
	}
}

func (w *Watcher) rebuild(ctx context.Context) error {
	in, err := ReadInputs(ctx, w.Cache, w.AdminNamespaces)
	if err != nil {
		return err
	}

	w.Store.Swap(Build(in))

	return nil
}

// ReadInputs lists everything a snapshot is built from. Secrets are read only
// in the admin namespaces.
func ReadInputs(ctx context.Context, reader client.Reader, adminNamespaces []string) (Inputs, error) {
	in := Inputs{AdminNamespaces: adminNamespaces}

	var apps githubgatev1alpha1.GitHubAppList

	var installations githubgatev1alpha1.GitHubInstallationList

	var policies githubgatev1alpha1.GitHubAccessPolicyList

	var users githubgatev1alpha1.GitHubUserAuthorizationList

	var namespaces corev1.NamespaceList

	for _, list := range []client.ObjectList{&apps, &installations, &policies, &users, &namespaces} {
		if err := reader.List(ctx, list); err != nil {
			return Inputs{}, fmt.Errorf("listing %T: %w", list, err)
		}
	}

	for _, namespace := range adminNamespaces {
		var secrets corev1.SecretList
		if err := reader.List(ctx, &secrets, client.InNamespace(namespace)); err != nil {
			return Inputs{}, fmt.Errorf("listing Secrets in %s: %w", namespace, err)
		}

		in.Secrets = append(in.Secrets, secrets.Items...)
	}

	in.Apps, in.Installations, in.Policies, in.Namespaces = apps.Items, installations.Items, policies.Items, namespaces.Items
	in.UserAuthorizations = users.Items

	return in, nil
}
