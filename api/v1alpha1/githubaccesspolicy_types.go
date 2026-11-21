package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TokenMode configures token mode for a rule.
type TokenMode struct {
	// Enabled turns token mode on for the rule. It has no default.
	Enabled bool `json:"enabled"`
}

// RefPattern is a git ref, or a pattern of refs, starting with `refs/`. A `*`
// matches one or more characters within one segment; a `**` segment matches
// one or more whole segments. The webhook also rejects what
// `git check-ref-format` refuses, such as `..` and `@{`.
//
// +kubebuilder:validation:MinLength=6
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^refs/[^\x00-\x20\x7f~^:?\[\\]+$`
type RefPattern string

// OperationPattern is a GitHub REST operation id, such as
// `repos/create-release`, or a pattern of them. A `*` matches one or more
// characters within one segment.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[a-z0-9*/-]+$`
type OperationPattern string

// MutationPattern is a GitHub GraphQL mutation name, such as
// `mergePullRequest`, or a pattern of them. A `*` matches one or more
// characters.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9*]+$`
type MutationPattern string

// ProxyMode configures proxy mode for a rule.
type ProxyMode struct {
	// Enabled turns proxy mode on for the rule. It has no default.
	Enabled bool `json:"enabled"`

	// PushRefs, when set, are the only refs a request may write. At most 256.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	PushRefs []RefPattern `json:"pushRefs,omitempty"`

	// AllowOperations, when set, are the only REST operations allowed.
	// At most 256.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	AllowOperations []OperationPattern `json:"allowOperations,omitempty"`

	// DenyOperations are REST operations that are refused. Deny wins over
	// allow. At most 256.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	DenyOperations []OperationPattern `json:"denyOperations,omitempty"`

	// AllowMutations, when set, are the only GraphQL mutations allowed.
	// At most 256.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	AllowMutations []MutationPattern `json:"allowMutations,omitempty"`

	// DenyMutations are GraphQL mutations that are refused. Deny wins over
	// allow. At most 256.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	DenyMutations []MutationPattern `json:"denyMutations,omitempty"`

	// PublicRead, when true, lets read-only requests reach repositories of
	// owners outside the installation, and read-only REST endpoints that name
	// no repository. They are sent with a token of the installation that has
	// only `metadata: read`, so they reach public data only. Off by default.
	//
	// +optional
	PublicRead bool `json:"publicRead,omitempty"`

	// WebPaths, when true, forwards `GET` and `HEAD` requests on github.com
	// that are not git smart HTTP, such as release downloads, with no
	// credential. Off by default.
	//
	// +optional
	WebPaths bool `json:"webPaths,omitempty"`
}

// AccessRule gives the policy's ServiceAccounts access through one
// installation, or, as a user rule, through one user authorization.
//
// +kubebuilder:validation:XValidation:rule="has(self.installationRef) != has(self.userAuthorizationRef)",message="exactly one of installationRef and userAuthorizationRef must be set"
// +kubebuilder:validation:XValidation:rule="has(self.userAuthorizationRef) || (has(self.repositories) && has(self.permissions))",message="a rule with installationRef needs repositories and permissions"
// +kubebuilder:validation:XValidation:rule="!has(self.userAuthorizationRef) || (!has(self.repositories) && !has(self.permissions))",message="a user rule has no repositories or permissions: its user authorization's operations replace them"
// +kubebuilder:validation:XValidation:rule="!has(self.userAuthorizationRef) || ((!has(self.token) || !self.token.enabled) && has(self.proxy) && self.proxy.enabled)",message="a user rule needs token mode off and proxy mode on: a user access token is never handed out"
// +kubebuilder:validation:XValidation:rule="!has(self.userAuthorizationRef) || !has(self.proxy) || (has(self.proxy.allowOperations) && self.proxy.allowOperations.all(o, !o.contains('*')))",message="a user rule needs proxy.allowOperations, with exact operation ids and no globs"
// +kubebuilder:validation:XValidation:rule="!has(self.userAuthorizationRef) || !has(self.proxy) || !(has(self.proxy.denyOperations) || has(self.proxy.allowMutations) || has(self.proxy.denyMutations) || has(self.proxy.pushRefs) || has(self.proxy.publicRead) || has(self.proxy.webPaths))",message="a user rule may set only proxy.enabled and proxy.allowOperations"
// +kubebuilder:validation:XValidation:rule="(has(self.token) && self.token.enabled) || (has(self.proxy) && self.proxy.enabled)",message="at least one mode must be enabled"
// +kubebuilder:validation:XValidation:rule="!(has(self.token) && self.token.enabled) || !has(self.proxy) || !(has(self.proxy.pushRefs) || has(self.proxy.allowOperations) || has(self.proxy.denyOperations) || has(self.proxy.allowMutations) || has(self.proxy.denyMutations))",message="proxy.pushRefs and the proxy operation and mutation lists need token mode off: a token in the caller's hands can't enforce them"
// +kubebuilder:validation:XValidation:rule="!has(self.proxy) || !has(self.proxy.publicRead) || !self.proxy.publicRead || !has(self.repositories) || self.repositories.exists(r, !r.contains('*'))",message="proxy.publicRead needs at least one repository named without a pattern: the public-read token is limited to those repositories"
type AccessRule struct {
	// Name identifies the rule within the policy.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// InstallationRef is the GitHubInstallation the rule draws on.
	//
	// +optional
	InstallationRef *ObjectReference `json:"installationRef,omitempty"`

	// UserAuthorizationRef is the GitHubUserAuthorization a user rule draws
	// on. A user rule is proxy-only and lists exact operations in
	// proxy.allowOperations.
	//
	// +optional
	UserAuthorizationRef *ObjectReference `json:"userAuthorizationRef,omitempty"`

	// Repositories are names or `*` globs under the installation's owner.
	// At most 256. A rule with installationRef needs them.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Repositories []Repository `json:"repositories,omitempty"`

	// Permissions maps GitHub App permission names to levels. A rule with
	// installationRef needs them.
	//
	// +optional
	// +kubebuilder:validation:MinProperties=1
	Permissions map[string]Level `json:"permissions,omitempty"`

	// Token configures token mode.
	//
	// +optional
	Token *TokenMode `json:"token,omitempty"`

	// Proxy configures proxy mode.
	//
	// +optional
	Proxy *ProxyMode `json:"proxy,omitempty"`
}

// GitHubAccessPolicySpec is the desired state of a GitHubAccessPolicy.
type GitHubAccessPolicySpec struct {
	// ServiceAccounts are names of ServiceAccounts in this namespace.
	// At most 256.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	ServiceAccounts []string `json:"serviceAccounts"`

	// Rules are keyed by name. They have no order and only add access.
	// At most 256.
	//
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	Rules []AccessRule `json:"rules,omitempty"`
}

// RuleStatus is the observed state of one rule.
type RuleStatus struct {
	// Name is the rule's name.
	Name string `json:"name"`

	// Conditions include `Accepted`.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitHubAccessPolicyStatus is the observed state of a GitHubAccessPolicy.
type GitHubAccessPolicyStatus struct {
	// ObservedGeneration is the generation the status describes.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Rules has one entry per rule, keyed by name.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	Rules []RuleStatus `json:"rules,omitempty"`

	// Conditions include `Ready`.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitHubAccessPolicy says what each ServiceAccount of its namespace may get.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=githubgate
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type GitHubAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitHubAccessPolicySpec   `json:"spec"`
	Status GitHubAccessPolicyStatus `json:"status,omitempty"`
}

// GitHubAccessPolicyList is a list of GitHubAccessPolicy.
//
// +kubebuilder:object:root=true
type GitHubAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubAccessPolicy `json:"items"`
}
