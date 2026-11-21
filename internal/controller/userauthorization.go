package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

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

	// clearRetry is how soon a failed clear of a changed spec's tokens is
	// tried again.
	clearRetry = 10 * time.Second
)

func setupUserAuthorizations(manager ctrl.Manager, b base) error {
	flows := newFlows()
	if err := manager.Add(flows); err != nil {
		return err
	}

	metrics := b.options.Metrics
	if metrics == nil {
		metrics = NewMetrics()
	}

	return ctrl.NewControllerManagedBy(manager).Named("githubuserauthorization-status").
		For(&githubgatev1alpha1.GitHubUserAuthorization{}).
		Watches(&githubgatev1alpha1.GitHubApp{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(b.allUserAuthorizations)).
		Complete(&userAuthorizationReconciler{
			base: b, flows: flows, workers: newWorkers(), metrics: metrics, apiReader: manager.GetAPIReader(),
			log: manager.GetLogger().WithName("githubuserauthorization"),
		})
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

type userAuthorizationReconciler struct {
	base
	flows     *flows
	workers   *workers
	metrics   *Metrics
	apiReader client.Reader
	log       logr.Logger
}

func (r *userAuthorizationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var object githubgatev1alpha1.GitHubUserAuthorization
	if err := r.client.Get(ctx, req.NamespacedName, &object); err != nil {
		if apierrors.IsNotFound(err) {
			r.flows.stop(req.NamespacedName)
			r.metrics.forget(req.NamespacedName)
		}

		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation

	authorization, found := snapshot.UserAuthorization(req.NamespacedName)

	var requeue time.Duration

	switch {
	case !found:
		r.flows.stop(req.NamespacedName)
		setReady(&status.Conditions, object.Generation, false, model.ReasonNotAllowed, "the GitHubUserAuthorization is not in an admin namespace")
	case !authorization.Resolved:
		setReady(&status.Conditions, object.Generation, false, authorization.Reason, authorization.Message)
	default:
		state, _, err := readUserTokens(ctx, r.client, &object, authorization)
		if err != nil {
			return reconcile.Result{}, err
		}

		request := object.Annotations[AnnotationAuthorize]

		switch {
		case state == tokenForeign:
			r.flows.stop(req.NamespacedName)
			setReady(&status.Conditions, object.Generation, false, model.ReasonSecretInvalid, foreignSecretMessage(object.Name))
		case request != "" && request != status.LastAuthorizeRequest:
			return reconcile.Result{}, r.startFlow(ctx, &object, authorization, request)
		case status.Authorization != nil && r.flows.runs(req.NamespacedName, status.Authorization.Request):
			// The flow's goroutine writes its end.
		case status.Authorization != nil:
			// The device code was only in the memory of a leader that is
			// gone; a new flow needs a new annotation value.
			status.Authorization = nil
			setOutcome(&status.Conditions, object.Generation, ReasonFlowLost,
				"the device flow was lost when githubgate restarted or its leader changed; set the annotation again")
		case state == tokenValid:
			// Only the leader runs this reconciler, so only the leader
			// refreshes.
			failed := failedAttempt(status.Conditions)
			force, wait := wakeRefresh(status)
			requeue = r.refreshStatus(req.NamespacedName, status, object.Generation, r.refreshIfDue(ctx, &object, authorization, force))

			if wait > 0 && (requeue == 0 || wait < requeue) {
				requeue = wait
			}

			if ready := apimeta.FindStatusCondition(status.Conditions, ConditionReady); failed != "" && ready != nil && ready.Status == metav1.ConditionTrue {
				setAttemptFailed(status, object.Generation, failed)
			}
		case state == tokenSpecChanged:
			// The status is written either way; a failed clear is tried
			// again soon.
			if err := r.clearSpecChanged(ctx, &object, authorization); err != nil {
				r.log.Info("the stored tokens of a changed spec could not be cleared", "authorization", req.String())

				requeue = clearRetry
			}

			setOutcome(&status.Conditions, object.Generation, ReasonSpecChanged,
				"the stored token was authorized for another login, App or client ID; it was cleared; set the annotation again")
		case flowOutcomes[readyReason(status.Conditions)]:
			// Keep how the last flow ended until something new happens.
		default:
			setReady(&status.Conditions, object.Generation, false, ReasonAuthorizationRequired,
				"no user token is stored; the user has to authorize the App")
			setAuthorizationRequired(&status.Conditions, object.Generation)
		}
	}

	// A configuration problem leaves AuthorizationRequired as it is, but an
	// authorization that never had it was never authorized either.
	if apimeta.FindStatusCondition(status.Conditions, ConditionAuthorizationRequired) == nil {
		setAuthorizationRequired(&status.Conditions, object.Generation)
	}

	r.metrics.observe(req.NamespacedName, status)

	return reconcile.Result{RequeueAfter: requeue}, update(ctx, r.client, &object, &object.Status, status)
}

// attemptFailures are the outcomes of a re-authorization that leave a valid
// stored token in use; the failure stays visible until a new flow starts.
var attemptFailures = map[string]bool{
	ReasonAuthorizationExpired: true,
	ReasonAuthorizationDenied:  true,
	ReasonDeviceFlowDisabled:   true,
	ReasonTokenExpiryDisabled:  true,
	ReasonWrongUser:            true,
	ReasonFlowLost:             true,
}

// failedAttempt returns the reason a finished flow failed with, or "".
func failedAttempt(conditions []metav1.Condition) string {
	if required := apimeta.FindStatusCondition(conditions, ConditionAuthorizationRequired); required != nil && attemptFailures[required.Reason] {
		return required.Reason
	}

	return ""
}

// setAttemptFailed reports a failed flow while the stored token stays in
// use: Ready stays true and AuthorizationRequired stays false, and both name
// the failure.
func setAttemptFailed(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64, reason string) {
	message := "the stored token of " + status.Login + " is still in use; the last authorization attempt (request " +
		status.LastAuthorizeRequest + ") failed: " + reason

	setReady(&status.Conditions, generation, true, model.ReasonReady, message)
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: ConditionAuthorizationRequired, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

func readyReason(conditions []metav1.Condition) string {
	if condition := apimeta.FindStatusCondition(conditions, ConditionReady); condition != nil {
		return condition.Reason
	}

	return ""
}

func setAuthorizationRequired(conditions *[]metav1.Condition, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: ConditionAuthorizationRequired, Status: metav1.ConditionTrue, Reason: ReasonNotAuthorized,
		Message: "the App was never authorized as this user", ObservedGeneration: generation,
	})
}
