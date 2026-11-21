package controller

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	// ConditionAuthorizationRequired is set on a user authorization that
	// has no usable token.
	ConditionAuthorizationRequired = "AuthorizationRequired"

	// ReasonAuthorizationRequired is the Ready reason of a user
	// authorization that has no usable token.
	ReasonAuthorizationRequired = "AuthorizationRequired"
	// ReasonNotAuthorized is the AuthorizationRequired reason of a user
	// authorization that was never authorized.
	ReasonNotAuthorized = "NotAuthorized"
)

func setupUserAuthorizations(manager ctrl.Manager, b base) error {
	return ctrl.NewControllerManagedBy(manager).Named("githubuserauthorization-status").
		For(&githubgatev1alpha1.GitHubUserAuthorization{}).
		Watches(&githubgatev1alpha1.GitHubApp{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Complete(&userAuthorizationReconciler{b})
}

func (b base) allUserAuthorizations(ctx context.Context, _ client.Object) []reconcile.Request {
	var list githubgatev1alpha1.GitHubUserAuthorizationList
	if err := b.client.List(ctx, &list); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, authorization := range list.Items {
		requests = append(requests, request(authorization.Namespace, authorization.Name))
	}

	return requests
}

type userAuthorizationReconciler struct{ base }

func (r *userAuthorizationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var object githubgatev1alpha1.GitHubUserAuthorization
	if err := r.client.Get(ctx, req.NamespacedName, &object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation

	authorization, found := snapshot.UserAuthorization(req.NamespacedName)

	switch {
	case !found:
		setReady(&status.Conditions, object.Generation, false, model.ReasonNotAllowed, "the GitHubUserAuthorization is not in an admin namespace")
	case !authorization.Resolved:
		setReady(&status.Conditions, object.Generation, false, authorization.Reason, authorization.Message)
	default:
		// No token is stored yet, so the user still has to authorize the App.
		setReady(&status.Conditions, object.Generation, false, ReasonAuthorizationRequired,
			"no user token is stored; the user has to authorize the App")
		setAuthorizationRequired(&status.Conditions, object.Generation)
	}

	// A configuration problem leaves AuthorizationRequired as it is, but an
	// authorization that never had it was never authorized either.
	if apimeta.FindStatusCondition(status.Conditions, ConditionAuthorizationRequired) == nil {
		setAuthorizationRequired(&status.Conditions, object.Generation)
	}

	return reconcile.Result{}, update(ctx, r.client, &object, &object.Status, status)
}

func setAuthorizationRequired(conditions *[]metav1.Condition, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: ConditionAuthorizationRequired, Status: metav1.ConditionTrue, Reason: ReasonNotAuthorized,
		Message: "the App was never authorized as this user", ObservedGeneration: generation,
	})
}
