package model

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/patterns"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

// Condition reasons the model decides.
const (
	ReasonReady             = "Ready"
	ReasonAccepted          = "Accepted"
	ReasonInvalidSpec       = "InvalidSpec"
	ReasonRefNotFound       = "RefNotFound"
	ReasonNotAllowed        = "NotAllowed"
	ReasonOutsideCeiling    = "OutsideCeiling"
	ReasonRefNotReady       = "RefNotReady"
	ReasonSecretUnavailable = "SecretUnavailable"
	ReasonSecretInvalid     = "SecretInvalid"
)

// Acceptance is the decision on one policy rule.
type Acceptance struct {
	Accepted bool
	Reason   string
	Message  string
}

// View is what deciding acceptance needs to look up.
type View interface {
	// Installation returns a GitHubInstallation in an admin namespace.
	Installation(name types.NamespacedName) (*Installation, bool)
	// NamespaceLabels returns a Namespace's labels, if the Namespace exists.
	NamespaceLabels(namespace string) (labels.Set, bool)
}

// AcceptRule decides whether a rule of a policy in policyNamespace is
// accepted. The first failing check wins: an invalid spec, then a missing
// installation, then an installation that doesn't admit the namespace, then a
// rule outside the installation's ceiling. A rule whose installation exists
// but isn't ready is still accepted, with reason RefNotReady.
func AcceptRule(policyNamespace string, rule githubgatev1alpha1.AccessRule, view View) Acceptance {
	if message := invalidRule(rule); message != "" {
		return Acceptance{Reason: ReasonInvalidSpec, Message: message}
	}

	ref := reference(rule.InstallationRef, policyNamespace)

	installation, ok := view.Installation(ref)
	if !ok {
		return Acceptance{Reason: ReasonRefNotFound, Message: fmt.Sprintf("GitHubInstallation %s not found", ref)}
	}

	if !Admits(installation.AllowedNamespaces, installation.Namespace, policyNamespace, view) {
		return Acceptance{
			Reason:  ReasonNotAllowed,
			Message: fmt.Sprintf("GitHubInstallation %s does not admit namespace %s", ref, policyNamespace),
		}
	}

	if !permissions.Contained(GrantOf(rule.Repositories, rule.Permissions), installation.Ceiling) {
		return Acceptance{
			Reason:  ReasonOutsideCeiling,
			Message: fmt.Sprintf("the rule is outside the ceiling of GitHubInstallation %s", ref),
		}
	}

	if !installation.Ready {
		return Acceptance{
			Accepted: true,
			Reason:   ReasonRefNotReady,
			Message:  fmt.Sprintf("GitHubInstallation %s is not ready: %s", ref, installation.Reason),
		}
	}

	return Acceptance{Accepted: true, Reason: ReasonAccepted, Message: "the rule is accepted"}
}

// Admits reports whether an object in ownNamespace with the given
// allowedNamespaces may be referenced from namespace. The object's own
// namespace is always admitted. A selector that doesn't parse, or a Namespace
// that doesn't exist, admits nothing.
func Admits(allowed githubgatev1alpha1.AllowedNamespaces, ownNamespace, namespace string, view View) bool {
	if namespace == ownNamespace {
		return true
	}

	for _, name := range allowed.Names {
		if name == namespace {
			return true
		}
	}

	if allowed.Selector == nil {
		return false
	}

	selector, err := metav1.LabelSelectorAsSelector(allowed.Selector)
	if err != nil {
		return false
	}

	namespaceLabels, ok := view.NamespaceLabels(namespace)
	if !ok {
		return false
	}

	return selector.Matches(namespaceLabels)
}

func invalidRule(rule githubgatev1alpha1.AccessRule) string {
	token := rule.Token != nil && rule.Token.Enabled
	if !token && (rule.Proxy == nil || !rule.Proxy.Enabled) {
		return "the rule enables no mode"
	}

	if token && hasProxyLists(rule.Proxy) {
		return "proxy.pushRefs and the proxy operation and mutation lists need token mode off"
	}

	if field, message := InvalidProxyMode(rule.Proxy); field != "" {
		return field + ": " + message
	}

	if message := invalidPermissionSet(rule.Repositories, rule.Permissions); message != "" {
		return message
	}

	return ""
}

// invalidPermissionSet describes the first problem with a set of repository
// patterns and permissions, or returns "" when there is none.
func invalidPermissionSet(repositories []githubgatev1alpha1.Repository, perms map[string]githubgatev1alpha1.Level) string {
	if len(repositories) == 0 {
		return "no repositories"
	}

	for _, repository := range repositories {
		if !patterns.Valid(string(repository)) {
			return fmt.Sprintf("repository pattern %q is not valid", repository)
		}
	}

	if len(perms) == 0 {
		return "no permissions"
	}

	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		level := permissions.Level(perms[name])
		if !permissions.ValidRepositoryPermission(name, level) {
			return fmt.Sprintf("permission %q at level %q is not a known repository permission level", name, level)
		}
	}

	return ""
}

// GrantOf converts repository patterns and permissions to a Grant.
func GrantOf(repositories []githubgatev1alpha1.Repository, perms map[string]githubgatev1alpha1.Level) permissions.Grant {
	grant := permissions.Grant{
		Repositories: make([]string, 0, len(repositories)),
		Permissions:  make(map[string]permissions.Level, len(perms)),
	}

	for _, repository := range repositories {
		grant.Repositories = append(grant.Repositories, string(repository))
	}

	for name, level := range perms {
		grant.Permissions[name] = permissions.Level(level)
	}

	return grant
}

func reference(ref githubgatev1alpha1.ObjectReference, defaultNamespace string) types.NamespacedName {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	return types.NamespacedName{Namespace: namespace, Name: ref.Name}
}
