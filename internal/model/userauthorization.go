package model

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/operations"
)

// DefaultClientIDKey is the default Secret key of a GitHubApp's client ID.
const DefaultClientIDKey = "client-id"

// UserAuthorization is a usable view of a GitHubUserAuthorization. Resolved
// says its App and the App's client ID were found; it says nothing about
// whether a user token is stored.
type UserAuthorization struct {
	types.NamespacedName
	App               *App
	ClientID          string
	Login             string
	Operations        []string
	AllowedNamespaces githubgatev1alpha1.AllowedNamespaces
	Resolved          bool
	Reason            string
	Message           string
}

// InvalidUserOperation describes why id can't be called as a user, or
// returns "" when it can.
func InvalidUserOperation(id string) string {
	if !operations.UserOperation(id) {
		return fmt.Sprintf("%q is not a REST operation in GitHub's REST API description whose path names no owner, repository, organization or enterprise", id)
	}

	return ""
}

func buildUserAuthorization(
	object *githubgatev1alpha1.GitHubUserAuthorization,
	snapshot *Snapshot,
	secrets map[types.NamespacedName]*corev1.Secret,
) *UserAuthorization {
	authorization := &UserAuthorization{
		NamespacedName:    types.NamespacedName{Namespace: object.Namespace, Name: object.Name},
		Login:             object.Spec.Login,
		AllowedNamespaces: object.Spec.AllowedNamespaces,
	}

	for index, operation := range object.Spec.Ceiling.Operations {
		if message := InvalidUserOperation(string(operation)); message != "" {
			// Never resolved, so the authorization grants nothing.
			return authorization.unresolved(ReasonInvalidSpec, fmt.Sprintf("ceiling.operations[%d]: %s", index, message))
		}

		authorization.Operations = append(authorization.Operations, string(operation))
	}

	ref := reference(object.Spec.AppRef, object.Namespace)

	app, ok := snapshot.App(ref)
	if !ok {
		return authorization.unresolved(ReasonRefNotFound, fmt.Sprintf("GitHubApp %s not found", ref))
	}

	if !Admits(app.AllowedNamespaces, app.Namespace, object.Namespace, snapshot) {
		return authorization.unresolved(ReasonNotAllowed, fmt.Sprintf("GitHubApp %s does not admit namespace %s", ref, object.Namespace))
	}

	authorization.App = app

	if !app.Ready {
		return authorization.unresolved(ReasonRefNotReady, fmt.Sprintf("GitHubApp %s is not ready: %s", ref, app.Reason))
	}

	switch {
	case app.clientIDSecretRef != nil:
		value, _, reason, message := secretValue(app.Namespace, *app.clientIDSecretRef, DefaultClientIDKey, secrets)
		if reason != "" {
			return authorization.unresolved(reason, "the App's client ID: "+message)
		}

		authorization.ClientID = strings.TrimSpace(string(value))
		if authorization.ClientID == "" {
			return authorization.unresolved(ReasonSecretInvalid, "the App's client ID in the Secret is empty")
		}
	case app.clientID != "":
		authorization.ClientID = app.clientID
	default:
		return authorization.unresolved(ReasonInvalidSpec, fmt.Sprintf("GitHubApp %s has no clientID or clientIDSecretRef", ref))
	}

	authorization.Resolved = true
	authorization.Reason = ReasonReady

	return authorization
}

func (u *UserAuthorization) unresolved(reason, message string) *UserAuthorization {
	u.Reason, u.Message = reason, message
	return u
}

// acceptUserRule decides a rule with userAuthorizationRef. The first failing
// check wins: an invalid spec, then a missing authorization, then an
// authorization that doesn't admit the namespace, then an operation outside
// its ceiling. A rule whose authorization exists but isn't resolved is still
// accepted, with reason RefNotReady.
func acceptUserRule(policyNamespace string, rule githubgatev1alpha1.AccessRule, view View) Acceptance {
	if message := invalidUserRule(rule); message != "" {
		return Acceptance{Reason: ReasonInvalidSpec, Message: message}
	}

	ref := reference(*rule.UserAuthorizationRef, policyNamespace)

	authorization, ok := view.UserAuthorization(ref)
	if !ok {
		return Acceptance{Reason: ReasonRefNotFound, Message: fmt.Sprintf("GitHubUserAuthorization %s not found", ref)}
	}

	if !Admits(authorization.AllowedNamespaces, authorization.Namespace, policyNamespace, view) {
		return Acceptance{
			Reason:  ReasonNotAllowed,
			Message: fmt.Sprintf("GitHubUserAuthorization %s does not admit namespace %s", ref, policyNamespace),
		}
	}

	for _, operation := range rule.Proxy.AllowOperations {
		if !containsString(authorization.Operations, string(operation)) {
			return Acceptance{
				Reason:  ReasonOutsideCeiling,
				Message: fmt.Sprintf("operation %q is outside the ceiling of GitHubUserAuthorization %s", operation, ref),
			}
		}
	}

	if !authorization.Resolved {
		return Acceptance{
			Accepted: true,
			Reason:   ReasonRefNotReady,
			Message:  fmt.Sprintf("GitHubUserAuthorization %s is not ready: %s", ref, authorization.Reason),
		}
	}

	return Acceptance{Accepted: true, Reason: ReasonAccepted, Message: "the rule is accepted"}
}

func invalidUserRule(rule githubgatev1alpha1.AccessRule) string {
	switch {
	case rule.InstallationRef != nil:
		return "a user rule can't also have installationRef"
	case len(rule.Repositories) > 0 || len(rule.Permissions) > 0:
		return "a user rule has no repositories or permissions"
	case rule.Token != nil && rule.Token.Enabled:
		return "a user rule needs token mode off"
	case rule.Proxy == nil || !rule.Proxy.Enabled:
		return "a user rule needs proxy mode on"
	case len(rule.Proxy.AllowOperations) == 0:
		return "a user rule needs proxy.allowOperations"
	case len(rule.Proxy.DenyOperations) > 0 || len(rule.Proxy.AllowMutations) > 0 || len(rule.Proxy.DenyMutations) > 0 ||
		len(rule.Proxy.PushRefs) > 0 || rule.Proxy.PublicRead || rule.Proxy.WebPaths:
		return "a user rule may set only proxy.enabled and proxy.allowOperations"
	}

	for index, operation := range rule.Proxy.AllowOperations {
		if message := InvalidUserOperation(string(operation)); message != "" {
			return fmt.Sprintf("proxy.allowOperations[%d]: %s", index, message)
		}
	}

	return ""
}

func containsString(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}

	return false
}
