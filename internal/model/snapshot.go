// Package model builds the immutable snapshot of Apps, installations and
// accepted policy rules that serving and the status controller read.
package model

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

// Default Secret keys.
const (
	DefaultAppIDKey      = "app-id"
	DefaultPrivateKeyKey = "private-key.pem"
)

// Inputs are the objects a snapshot is built from.
type Inputs struct {
	// AdminNamespaces are the only namespaces whose Apps, installations and
	// Secrets are used.
	AdminNamespaces []string
	Apps            []githubgatev1alpha1.GitHubApp
	Installations   []githubgatev1alpha1.GitHubInstallation
	Policies        []githubgatev1alpha1.GitHubAccessPolicy
	// UserAuthorizations are used only in the admin namespaces.
	UserAuthorizations []githubgatev1alpha1.GitHubUserAuthorization
	Secrets            []corev1.Secret
	Namespaces         []corev1.Namespace
}

// App is a usable view of a GitHubApp.
type App struct {
	types.NamespacedName
	AllowedNamespaces githubgatev1alpha1.AllowedNamespaces
	AppID             int64
	// KeyVersion changes whenever the App ID or the key may have changed,
	// so clients built from them can be rebuilt.
	KeyVersion string
	Ready      bool
	Reason     string
	Message    string

	privateKey *rsa.PrivateKey
	// The client ID, inline or from a Secret, is resolved by the user
	// authorizations that need it, so a missing one never makes the App
	// not ready.
	clientID          string
	clientIDSecretRef *githubgatev1alpha1.SecretKeyReference
}

// PrivateKey returns the App's parsed private key, or nil when not ready.
// It is only for signing App JWTs and must never be logged.
func (a *App) PrivateKey() *rsa.PrivateKey { return a.privateKey }

// Installation is a usable view of a GitHubInstallation.
type Installation struct {
	types.NamespacedName
	Owner             string
	App               *App
	Ceiling           []permissions.Grant
	AllowedNamespaces githubgatev1alpha1.AllowedNamespaces
	Ready             bool
	Reason            string
	Message           string
}

// Rule is a policy rule a caller can use: an accepted installation rule, or
// a user rule, accepted or not. A user rule has no Installation.
type Rule struct {
	Policy       types.NamespacedName
	Name         string
	Installation *Installation
	Repositories []string
	Permissions  map[string]permissions.Level
	// Token is whether the rule enables token mode.
	Token bool
	// Proxy is the rule's proxy mode, or nil when it doesn't enable it.
	Proxy *ProxyRule
	// User is set on a user rule only.
	User *UserRule
}

// UserRule is what a rule with userAuthorizationRef calls as a user. A user
// rule that isn't accepted is kept so that the operations it lists are
// refused instead of being decided by the installation rules.
type UserRule struct {
	// Authorization is the GitHubUserAuthorization the rule references.
	Authorization types.NamespacedName
	// View is that authorization, or nil when it doesn't exist.
	View *UserAuthorization
	// Operations are the exact operation ids the rule lists.
	Operations []string
	// Accepted is the rule's acceptance; Reason says why it isn't.
	Accepted bool
	Reason   string
}

// PolicyRule is the acceptance decision on one rule of a policy.
type PolicyRule struct {
	Name string
	Acceptance
}

// Snapshot is an immutable view of the configuration. Never modify it.
type Snapshot struct {
	apps            map[types.NamespacedName]*App
	installations   map[types.NamespacedName]*Installation
	users           map[types.NamespacedName]*UserAuthorization
	namespaceLabels map[string]labels.Set
	// policiesOf lists, for each ServiceAccount, the accepted rules of each
	// policy that names it. Each policy's rules are stored once and shared,
	// so the snapshot grows with ServiceAccounts plus rules, never with
	// their product.
	policiesOf map[types.NamespacedName][][]*Rule
	policies   map[types.NamespacedName][]PolicyRule
}

// App returns a GitHubApp in an admin namespace.
func (s *Snapshot) App(name types.NamespacedName) (*App, bool) {
	app, ok := s.apps[name]
	return app, ok
}

// UserAuthorization returns a GitHubUserAuthorization in an admin namespace.
func (s *Snapshot) UserAuthorization(name types.NamespacedName) (*UserAuthorization, bool) {
	authorization, ok := s.users[name]
	return authorization, ok
}

// Installation returns a GitHubInstallation in an admin namespace.
func (s *Snapshot) Installation(name types.NamespacedName) (*Installation, bool) {
	installation, ok := s.installations[name]
	return installation, ok
}

// Installations returns every GitHubInstallation in an admin namespace.
func (s *Snapshot) Installations() []*Installation {
	installations := make([]*Installation, 0, len(s.installations))
	for _, installation := range s.installations {
		installations = append(installations, installation)
	}

	return installations
}

// NamespaceLabels returns a Namespace's labels, if the Namespace exists.
func (s *Snapshot) NamespaceLabels(namespace string) (labels.Set, bool) {
	set, ok := s.namespaceLabels[namespace]
	return set, ok
}

// Rules returns the rules that name a ServiceAccount, from the policies in
// the ServiceAccount's own namespace: the accepted installation rules and
// every user rule.
func (s *Snapshot) Rules(namespace, serviceAccount string) []*Rule {
	policies := s.policiesOf[types.NamespacedName{Namespace: namespace, Name: serviceAccount}]

	count := 0
	for _, rules := range policies {
		count += len(rules)
	}

	if count == 0 {
		return nil
	}

	joined := make([]*Rule, 0, count)
	for _, rules := range policies {
		joined = append(joined, rules...)
	}

	return joined
}

// PolicyRules returns the acceptance decision on each rule of a policy.
func (s *Snapshot) PolicyRules(policy types.NamespacedName) ([]PolicyRule, bool) {
	rules, ok := s.policies[policy]
	return rules, ok
}

// Build makes a snapshot from the inputs. Apps and installations outside the
// admin namespaces are ignored, and so are Secrets outside them.
func Build(in Inputs) *Snapshot {
	snapshot := &Snapshot{
		apps:            map[types.NamespacedName]*App{},
		installations:   map[types.NamespacedName]*Installation{},
		users:           map[types.NamespacedName]*UserAuthorization{},
		namespaceLabels: map[string]labels.Set{},
		policiesOf:      map[types.NamespacedName][][]*Rule{},
		policies:        map[types.NamespacedName][]PolicyRule{},
	}

	admin := func(namespace string) bool { return slices.Contains(in.AdminNamespaces, namespace) }

	for _, namespace := range in.Namespaces {
		snapshot.namespaceLabels[namespace.Name] = labels.Set(namespace.Labels)
	}

	secrets := map[types.NamespacedName]*corev1.Secret{}

	for index := range in.Secrets {
		secret := &in.Secrets[index]
		if admin(secret.Namespace) {
			secrets[types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}] = secret
		}
	}

	for index := range in.Apps {
		app := &in.Apps[index]
		if admin(app.Namespace) {
			snapshot.apps[types.NamespacedName{Namespace: app.Namespace, Name: app.Name}] = buildApp(app, secrets)
		}
	}

	for index := range in.Installations {
		installation := &in.Installations[index]
		if admin(installation.Namespace) {
			name := types.NamespacedName{Namespace: installation.Namespace, Name: installation.Name}
			snapshot.installations[name] = buildInstallation(installation, snapshot)
		}
	}

	for index := range in.UserAuthorizations {
		authorization := &in.UserAuthorizations[index]
		if admin(authorization.Namespace) {
			name := types.NamespacedName{Namespace: authorization.Namespace, Name: authorization.Name}
			snapshot.users[name] = buildUserAuthorization(authorization, snapshot, secrets)
		}
	}

	for index := range in.Policies {
		buildPolicy(&in.Policies[index], snapshot)
	}

	return snapshot
}

func buildApp(object *githubgatev1alpha1.GitHubApp, secrets map[types.NamespacedName]*corev1.Secret) *App {
	app := &App{
		NamespacedName:    types.NamespacedName{Namespace: object.Namespace, Name: object.Name},
		AllowedNamespaces: object.Spec.AllowedNamespaces,
		clientID:          object.Spec.ClientID,
		clientIDSecretRef: object.Spec.ClientIDSecretRef,
	}

	versions := []string{}

	switch {
	case object.Spec.AppID != nil:
		app.AppID = *object.Spec.AppID
	case object.Spec.AppIDSecretRef != nil:
		value, version, reason, message := secretValue(object.Namespace, *object.Spec.AppIDSecretRef, DefaultAppIDKey, secrets)
		if reason != "" {
			return app.notReady(reason, message)
		}

		versions = append(versions, version)

		id, err := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
		if err != nil || id < 1 {
			return app.notReady(ReasonSecretInvalid, "the App ID in the Secret is not a positive decimal number")
		}

		app.AppID = id
	}

	if app.AppID < 1 {
		return app.notReady(ReasonInvalidSpec, "the App ID is not set")
	}

	value, version, reason, message := secretValue(object.Namespace, object.Spec.PrivateKeySecretRef, DefaultPrivateKeyKey, secrets)
	if reason != "" {
		return app.notReady(reason, message)
	}

	key, err := parsePrivateKey(value)
	if err != nil {
		return app.notReady(ReasonSecretInvalid, "the private key in the Secret is not a PEM RSA key (PKCS#1 or PKCS#8)")
	}

	versions = append(versions, version)
	app.KeyVersion = fmt.Sprintf("%d/%s", app.AppID, strings.Join(versions, "/"))
	app.privateKey = key
	app.Ready = true
	app.Reason = ReasonReady

	return app
}

func (a *App) notReady(reason, message string) *App {
	a.Reason, a.Message = reason, message
	return a
}

func secretValue(
	namespace string,
	ref githubgatev1alpha1.SecretKeyReference,
	defaultKey string,
	secrets map[types.NamespacedName]*corev1.Secret,
) (value []byte, version, reason, message string) {
	key := ref.Key
	if key == "" {
		key = defaultKey
	}

	secret, ok := secrets[types.NamespacedName{Namespace: namespace, Name: ref.Name}]
	if !ok {
		return nil, "", ReasonSecretUnavailable, fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name)
	}

	value, ok = secret.Data[key]
	if !ok {
		return nil, "", ReasonSecretUnavailable, fmt.Sprintf("Secret %s/%s has no key %q", namespace, ref.Name, key)
	}

	return value, secret.ResourceVersion, "", ""
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a PKCS#1 or PKCS#8 key")
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}

	return key, nil
}

func buildInstallation(object *githubgatev1alpha1.GitHubInstallation, snapshot *Snapshot) *Installation {
	installation := &Installation{
		NamespacedName:    types.NamespacedName{Namespace: object.Namespace, Name: object.Name},
		Owner:             object.Spec.Owner,
		AllowedNamespaces: object.Spec.AllowedNamespaces,
	}

	for _, entry := range object.Spec.Ceiling {
		installation.Ceiling = append(installation.Ceiling, GrantOf(entry.Repositories, entry.Permissions))
	}

	for _, entry := range object.Spec.Ceiling {
		if message := invalidPermissionSet(entry.Repositories, entry.Permissions); message != "" {
			// Never ready, so the installation grants nothing.
			return installation.notReady(ReasonInvalidSpec, "ceiling: "+message)
		}
	}

	if len(object.Spec.Ceiling) == 0 {
		return installation.notReady(ReasonInvalidSpec, "the ceiling is empty")
	}

	ref := reference(object.Spec.AppRef, object.Namespace)

	app, ok := snapshot.App(ref)
	if !ok {
		return installation.notReady(ReasonRefNotFound, fmt.Sprintf("GitHubApp %s not found", ref))
	}

	if !Admits(app.AllowedNamespaces, app.Namespace, object.Namespace, snapshot) {
		return installation.notReady(ReasonNotAllowed, fmt.Sprintf("GitHubApp %s does not admit namespace %s", ref, object.Namespace))
	}

	installation.App = app

	if !app.Ready {
		return installation.notReady(ReasonRefNotReady, fmt.Sprintf("GitHubApp %s is not ready: %s", ref, app.Reason))
	}

	installation.Ready = true
	installation.Reason = ReasonReady

	return installation
}

func (i *Installation) notReady(reason, message string) *Installation {
	i.Reason, i.Message = reason, message
	return i
}

func buildPolicy(object *githubgatev1alpha1.GitHubAccessPolicy, snapshot *Snapshot) {
	policy := types.NamespacedName{Namespace: object.Namespace, Name: object.Name}
	decisions := make([]PolicyRule, 0, len(object.Spec.Rules))
	accepted := []*Rule{}

	for _, rule := range object.Spec.Rules {
		acceptance := AcceptRule(object.Namespace, rule, snapshot)
		decisions = append(decisions, PolicyRule{Name: rule.Name, Acceptance: acceptance})

		if rule.UserAuthorizationRef != nil {
			accepted = append(accepted, userRuleOf(policy, rule, acceptance, snapshot))

			continue
		}

		if !acceptance.Accepted || rule.InstallationRef == nil {
			continue
		}

		installation, _ := snapshot.Installation(reference(*rule.InstallationRef, object.Namespace))
		grant := GrantOf(rule.Repositories, rule.Permissions)
		accepted = append(accepted, &Rule{
			Policy:       policy,
			Name:         rule.Name,
			Installation: installation,
			Repositories: grant.Repositories,
			Permissions:  grant.Permissions,
			Token:        rule.Token != nil && rule.Token.Enabled,
			Proxy:        proxyRuleOf(rule.Proxy),
		})
	}

	snapshot.policies[policy] = decisions

	if len(accepted) == 0 {
		return
	}

	for _, serviceAccount := range slices.Compact(slices.Sorted(slices.Values(object.Spec.ServiceAccounts))) {
		key := types.NamespacedName{Namespace: object.Namespace, Name: serviceAccount}
		snapshot.policiesOf[key] = append(snapshot.policiesOf[key], accepted)
	}
}
