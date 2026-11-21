package controller

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
)

// wakeMinInterval is the shortest time between two refreshes that a refused
// access token asks for, and between two requests for one, so a burst of
// refusals causes one refresh.
const wakeMinInterval = 30 * time.Second

// RefreshRequester lets a replica ask the leader to refresh an
// authorization's token now. It writes only the status field the leader
// watches; it never refreshes itself.
type RefreshRequester struct {
	client client.Client

	mu   sync.Mutex
	last map[types.NamespacedName]time.Time
}

// NewRefreshRequester returns a requester that writes through c.
func NewRefreshRequester(c client.Client) *RefreshRequester {
	return &RefreshRequester{client: c, last: map[types.NamespacedName]time.Time{}}
}

// Request records that GitHub refused the access token of name. It does
// nothing when a request was made in the last wakeMinInterval.
func (r *RefreshRequester) Request(ctx context.Context, name types.NamespacedName) error {
	now := time.Now()

	r.mu.Lock()

	if previous, ok := r.last[name]; ok && now.Sub(previous) < wakeMinInterval {
		r.mu.Unlock()

		return nil
	}

	r.last[name] = now
	r.mu.Unlock()

	var object githubgatev1alpha1.GitHubUserAuthorization
	if err := r.client.Get(ctx, name, &object); err != nil {
		return client.IgnoreNotFound(err)
	}

	if asked := object.Status.RefreshRequestedAt; asked != nil && now.Sub(asked.Time) < wakeMinInterval {
		return nil
	}

	patch := client.MergeFrom(object.DeepCopy())
	object.Status.RefreshRequestedAt = &metav1.Time{Time: now.UTC().Truncate(time.Second)}

	return client.IgnoreNotFound(r.client.Status().Patch(ctx, &object, patch))
}

// wakeRefresh says whether the leader must refresh now because a replica saw
// the access token refused, and otherwise how long until it may.
func wakeRefresh(status *githubgatev1alpha1.GitHubUserAuthorizationStatus) (now bool, wait time.Duration) {
	asked := status.RefreshRequestedAt
	if asked == nil {
		return false, 0
	}

	if last := status.LastRefreshTime; last != nil {
		if !asked.After(last.Time) {
			return false, 0
		}

		if remaining := wakeMinInterval - time.Since(last.Time); remaining > 0 {
			return false, remaining
		}
	}

	return true, 0
}
