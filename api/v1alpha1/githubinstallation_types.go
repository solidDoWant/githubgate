package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitHubInstallationSpec is the desired state of a GitHubInstallation.
type GitHubInstallationSpec struct {
	// AppRef is the GitHubApp this installation belongs to.
	AppRef ObjectReference `json:"appRef"`

	// Owner is the user or organisation login the App is installed on.
	// It is compared case-insensitively.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	Owner string `json:"owner"`

	// Ceiling is the most any policy may get through this installation.
	//
	// +kubebuilder:validation:MinItems=1
	Ceiling []PermissionSet `json:"ceiling"`

	// AllowedNamespaces says who may reference this installation from a policy.
	//
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// GitHubInstallationStatus is the observed state of a GitHubInstallation.
type GitHubInstallationStatus struct {
	// ObservedGeneration is the generation the status describes.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// InstallationID is the installation's ID on GitHub.
	//
	// +optional
	InstallationID int64 `json:"installationID,omitempty"`

	// GrantedPermissions are the permissions GitHub reports for the installation.
	//
	// +optional
	GrantedPermissions map[string]Level `json:"grantedPermissions,omitempty"`

	// Conditions include `Ready` and `CeilingExceedsGrant`.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitHubInstallation is an App's installation on an owner and its ceiling.
// Admin namespaces only.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=githubgate
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type GitHubInstallation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitHubInstallationSpec   `json:"spec"`
	Status GitHubInstallationStatus `json:"status,omitempty"`
}

// GitHubInstallationList is a list of GitHubInstallation.
//
// +kubebuilder:object:root=true
type GitHubInstallationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubInstallation `json:"items"`
}
