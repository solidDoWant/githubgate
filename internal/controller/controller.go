// Package controller writes the status of GitHubApp, GitHubInstallation and
// GitHubAccessPolicy objects. Its reconcilers are leader-elected, so only one
// replica writes status. They decide acceptance with the same model code that
// serving uses.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

const (
	// ConditionReady is the condition every kind has.
	ConditionReady = "Ready"
	// ConditionAccepted is the condition of a policy rule.
	ConditionAccepted = "Accepted"
	// ConditionCeilingExceedsGrant is set on an installation whose ceiling is
	// above what GitHub granted the App.
	ConditionCeilingExceedsGrant = "CeilingExceedsGrant"

	// ReasonRulesNotAccepted is the policy reason when a rule isn't accepted.
	ReasonRulesNotAccepted = "RulesNotAccepted"
	// ReasonGitHubUnavailable is the reason when GitHub can't be asked.
	ReasonGitHubUnavailable = "GitHubUnavailable"

	// recheckInterval is how often GitHub is asked again about a ready App or
	// installation.
	recheckInterval = 10 * time.Minute
	// retryInterval is how soon a failed GitHub check is repeated.
	retryInterval = time.Minute
)

// Options configure the reconcilers.
type Options struct {
	// AdminNamespaces are the namespaces whose Apps, installations and Secrets are used.
	AdminNamespaces []string
	// GitHub makes the App clients used for the GitHub checks.
	GitHub *github.Registry
}

// Setup registers the three status reconcilers with the manager. They run
// only on the elected leader.
func Setup(manager ctrl.Manager, options Options) error {
	base := base{client: manager.GetClient(), options: options}

	allApps := func(ctx context.Context, object client.Object) []reconcile.Request {
		var list githubgatev1alpha1.GitHubAppList
		if err := base.client.List(ctx, &list); err != nil {
			return nil
		}

		var requests []reconcile.Request

		for _, app := range list.Items {
			if _, isNamespace := object.(*corev1.Namespace); isNamespace || app.Namespace == object.GetNamespace() {
				requests = append(requests, request(app.Namespace, app.Name))
			}
		}

		return requests
	}

	err := ctrl.NewControllerManagedBy(manager).Named("githubapp-status").
		For(&githubgatev1alpha1.GitHubApp{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(allApps)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(allApps)).
		Complete(&appReconciler{base})
	if err != nil {
		return err
	}

	err = ctrl.NewControllerManagedBy(manager).Named("githubinstallation-status").
		For(&githubgatev1alpha1.GitHubInstallation{}).
		Watches(&githubgatev1alpha1.GitHubApp{}, handler.EnqueueRequestsFromMapFunc(base.installationsOfApp)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(base.allInstallations)).
		Complete(&installationReconciler{base})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(manager).Named("githubaccesspolicy-status").
		For(&githubgatev1alpha1.GitHubAccessPolicy{}).
		Watches(&githubgatev1alpha1.GitHubInstallation{}, handler.EnqueueRequestsFromMapFunc(base.policiesOfInstallation)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(base.policiesInNamespace)).
		Complete(&policyReconciler{base})
}

type base struct {
	client  client.Client
	options Options
}

func request(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// snapshot builds the model from the cache, so a decision never lags the
// objects the reconciler just read.
func (b base) snapshot(ctx context.Context) (*model.Snapshot, error) {
	in, err := model.ReadInputs(ctx, b.client, b.options.AdminNamespaces)
	if err != nil {
		return nil, err
	}

	return model.Build(in), nil
}

func (b base) installationsOfApp(ctx context.Context, object client.Object) []reconcile.Request {
	var list githubgatev1alpha1.GitHubInstallationList
	if err := b.client.List(ctx, &list); err != nil {
		return nil
	}

	var requests []reconcile.Request

	for _, installation := range list.Items {
		ref := installation.Spec.AppRef

		namespace := ref.Namespace
		if namespace == "" {
			namespace = installation.Namespace
		}

		if namespace == object.GetNamespace() && ref.Name == object.GetName() {
			requests = append(requests, request(installation.Namespace, installation.Name))
		}
	}

	return requests
}

func (b base) allInstallations(ctx context.Context, _ client.Object) []reconcile.Request {
	var list githubgatev1alpha1.GitHubInstallationList
	if err := b.client.List(ctx, &list); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, installation := range list.Items {
		requests = append(requests, request(installation.Namespace, installation.Name))
	}

	return requests
}

func (b base) policiesOfInstallation(ctx context.Context, object client.Object) []reconcile.Request {
	var list githubgatev1alpha1.GitHubAccessPolicyList
	if err := b.client.List(ctx, &list); err != nil {
		return nil
	}

	var requests []reconcile.Request

	for _, policy := range list.Items {
		for _, rule := range policy.Spec.Rules {
			namespace := rule.InstallationRef.Namespace
			if namespace == "" {
				namespace = policy.Namespace
			}

			if namespace == object.GetNamespace() && rule.InstallationRef.Name == object.GetName() {
				requests = append(requests, request(policy.Namespace, policy.Name))
				break
			}
		}
	}

	return requests
}

func (b base) policiesInNamespace(ctx context.Context, object client.Object) []reconcile.Request {
	var list githubgatev1alpha1.GitHubAccessPolicyList
	if err := b.client.List(ctx, &list); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, policy := range list.Items {
		// A Namespace change can change what an installation admits, for any policy.
		requests = append(requests, request(policy.Namespace, policy.Name))
	}

	return requests
}

func setReady(conditions *[]metav1.Condition, generation int64, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}

	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: ConditionReady, Status: status, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

type appReconciler struct{ base }

func (r *appReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var object githubgatev1alpha1.GitHubApp
	if err := r.client.Get(ctx, req.NamespacedName, &object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	result := reconcile.Result{}

	app, found := snapshot.App(req.NamespacedName)

	switch {
	case !found:
		setReady(&status.Conditions, object.Generation, false, model.ReasonNotAllowed, "the GitHubApp is not in an admin namespace")
	case !app.Ready:
		setReady(&status.Conditions, object.Generation, false, app.Reason, app.Message)
	default:
		slug, err := r.slug(ctx, app)
		if err == nil {
			status.Slug = slug
			setReady(&status.Conditions, object.Generation, true, model.ReasonReady, "the App's credentials are accepted by GitHub")

			result.RequeueAfter = recheckInterval
		} else {
			reason, message := failure(err)
			setReady(&status.Conditions, object.Generation, false, reason, message)

			result.RequeueAfter = retryInterval
		}
	}

	return result, update(ctx, r.client, &object, &object.Status, status)
}

func (r *appReconciler) slug(ctx context.Context, app *model.App) (string, error) {
	client, err := r.options.GitHub.Client(app.NamespacedName, github.Credentials{
		AppID: app.AppID, PrivateKey: app.PrivateKey(), Version: app.KeyVersion,
	})
	if err != nil {
		return "", err
	}

	return client.App(ctx)
}

// failure maps a GitHub failure to a Ready reason and message.
func failure(err error) (reason, message string) {
	var githubError *github.Error
	if errors.As(err, &githubError) {
		if githubError.Condition != "" {
			return githubError.Condition, githubError.Message
		}

		return ReasonGitHubUnavailable, githubError.Message
	}

	return ReasonGitHubUnavailable, "GitHub could not be asked"
}

type installationReconciler struct{ base }

func (r *installationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var object githubgatev1alpha1.GitHubInstallation
	if err := r.client.Get(ctx, req.NamespacedName, &object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	result := reconcile.Result{}

	installation, found := snapshot.Installation(req.NamespacedName)

	switch {
	case !found:
		setReady(&status.Conditions, object.Generation, false, model.ReasonNotAllowed, "the GitHubInstallation is not in an admin namespace")
		clearGitHubState(status)
	case !installation.Ready:
		setReady(&status.Conditions, object.Generation, false, installation.Reason, installation.Message)
		clearGitHubState(status)
	default:
		result.RequeueAfter = r.check(ctx, installation, object.Generation, status)
	}

	return result, update(ctx, r.client, &object, &object.Status, status)
}

func clearGitHubState(status *githubgatev1alpha1.GitHubInstallationStatus) {
	status.InstallationID = 0
	status.GrantedPermissions = nil

	apimeta.RemoveStatusCondition(&status.Conditions, ConditionCeilingExceedsGrant)
}

// check asks GitHub about a model-ready installation, updates the status and
// returns when to ask again.
func (r *installationReconciler) check(
	ctx context.Context,
	installation *model.Installation,
	generation int64,
	status *githubgatev1alpha1.GitHubInstallationStatus,
) time.Duration {
	app := installation.App

	appClient, err := r.options.GitHub.Client(app.NamespacedName, github.Credentials{
		AppID: app.AppID, PrivateKey: app.PrivateKey(), Version: app.KeyVersion,
	})

	var info github.InstallationInfo
	if err == nil {
		info, err = appClient.Installation(ctx, installation.Owner)
	}

	if err != nil {
		reason, message := failure(err)
		setReady(&status.Conditions, generation, false, reason, message)

		if reason != ReasonGitHubUnavailable {
			clearGitHubState(status)
		}

		return retryInterval
	}

	status.InstallationID = info.ID
	status.GrantedPermissions = make(map[string]githubgatev1alpha1.Level, len(info.Permissions))

	for name, level := range info.Permissions {
		status.GrantedPermissions[name] = githubgatev1alpha1.Level(level)
	}

	exceeding := ceilingExceeds(installation.Ceiling, info.Permissions)
	if len(exceeding) > 0 {
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: ConditionCeilingExceedsGrant, Status: metav1.ConditionTrue, Reason: ConditionCeilingExceedsGrant,
			Message: fmt.Sprintf("the ceiling is above what GitHub granted the App for: %v", exceeding), ObservedGeneration: generation,
		})
	} else {
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: ConditionCeilingExceedsGrant, Status: metav1.ConditionFalse, Reason: "WithinGrant",
			Message: "the ceiling is within what GitHub granted the App", ObservedGeneration: generation,
		})
	}

	if info.Suspended {
		setReady(&status.Conditions, generation, false, github.ConditionInstallationSuspended, "the installation is suspended")
		return retryInterval
	}

	setReady(&status.Conditions, generation, true, model.ReasonReady, "the installation exists and is usable")

	return recheckInterval
}

// ceilingExceeds lists the permissions some ceiling entry asks for above the
// installation's grant.
func ceilingExceeds(ceiling []permissions.Grant, granted map[string]string) []string {
	var exceeding []string

	for _, entry := range ceiling {
		for name, level := range entry.Permissions {
			if !permissions.Level(granted[name]).AtLeast(level) && !slices.Contains(exceeding, name) {
				exceeding = append(exceeding, name)
			}
		}
	}

	slices.Sort(exceeding)

	return exceeding
}

type policyReconciler struct{ base }

func (r *policyReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var object githubgatev1alpha1.GitHubAccessPolicy
	if err := r.client.Get(ctx, req.NamespacedName, &object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	status.Rules = nil

	decisions, _ := snapshot.PolicyRules(req.NamespacedName)

	var notAccepted []string

	for _, decision := range decisions {
		ruleStatus := githubgatev1alpha1.RuleStatus{Name: decision.Name}
		accepted := metav1.ConditionTrue

		if !decision.Accepted {
			accepted = metav1.ConditionFalse

			notAccepted = append(notAccepted, decision.Name)
		}

		apimeta.SetStatusCondition(&ruleStatus.Conditions, metav1.Condition{
			Type: ConditionAccepted, Status: accepted, Reason: decision.Reason, Message: decision.Message, ObservedGeneration: object.Generation,
		})
		status.Rules = append(status.Rules, ruleStatus)
	}

	// Keep the transition times of unchanged rule conditions.
	for index, ruleStatus := range status.Rules {
		for _, previous := range object.Status.Rules {
			if previous.Name == ruleStatus.Name {
				merged := previous.DeepCopy()
				apimeta.SetStatusCondition(&merged.Conditions, ruleStatus.Conditions[0])
				status.Rules[index] = *merged
			}
		}
	}

	if len(notAccepted) == 0 {
		setReady(&status.Conditions, object.Generation, true, model.ReasonReady, "every rule is accepted")
	} else {
		setReady(&status.Conditions, object.Generation, false, ReasonRulesNotAccepted,
			fmt.Sprintf("rules not accepted: %v; the accepted rules still apply", notAccepted))
	}

	return reconcile.Result{}, update(ctx, r.client, &object, &object.Status, status)
}

// update writes a status when it differs from the current one.
func update[S any](ctx context.Context, c client.Client, object client.Object, current *S, desired *S) error {
	if equality.Semantic.DeepEqual(current, desired) {
		return nil
	}

	*current = *desired

	return c.Status().Update(ctx, object)
}
