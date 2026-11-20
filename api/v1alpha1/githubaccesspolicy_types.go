package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TokenMode configures token mode for a rule.
type TokenMode struct {
	// Enabled turns token mode on for the rule. It has no default.
	Enabled bool `json:"enabled"`
}

// AccessRule gives the policy's ServiceAccounts access through one installation.
//
// +kubebuilder:validation:XValidation:rule="has(self.token) && self.token.enabled",message="at least one mode must be enabled"
type AccessRule struct {
	// Name identifies the rule within the policy.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// InstallationRef is the GitHubInstallation the rule draws on.
	InstallationRef ObjectReference `json:"installationRef"`

	// Repositories are names or `*` globs under the installation's owner.
	// At most 256.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Repositories []Repository `json:"repositories"`

	// Permissions maps GitHub App permission names to levels.
	//
	// +kubebuilder:validation:MinProperties=1
	Permissions map[string]Level `json:"permissions"`

	// Token configures token mode.
	Token *TokenMode `json:"token"`
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
