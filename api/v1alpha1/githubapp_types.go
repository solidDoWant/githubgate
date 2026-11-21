package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitHubAppSpec is the desired state of a GitHubApp.
//
// +kubebuilder:validation:XValidation:rule="has(self.appID) != has(self.appIDSecretRef)",message="exactly one of appID and appIDSecretRef must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.clientID) && has(self.clientIDSecretRef))",message="at most one of clientID and clientIDSecretRef may be set"
type GitHubAppSpec struct {
	// AppID is the App's ID, set inline.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	AppID *int64 `json:"appID,omitempty"`

	// AppIDSecretRef names a Secret key holding the App ID as a decimal
	// string. The key defaults to `app-id`.
	//
	// +optional
	AppIDSecretRef *SecretKeyReference `json:"appIDSecretRef,omitempty"`

	// ClientID is the App's OAuth client ID, set inline. Only a
	// GitHubUserAuthorization of the App needs it.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ClientID string `json:"clientID,omitempty"`

	// ClientIDSecretRef names a Secret key holding the App's OAuth client
	// ID. The key defaults to `client-id`.
	//
	// +optional
	ClientIDSecretRef *SecretKeyReference `json:"clientIDSecretRef,omitempty"`

	// PrivateKeySecretRef names a Secret key holding the App's PEM private
	// key (PKCS#1 or PKCS#8 RSA). The key defaults to `private-key.pem`.
	PrivateKeySecretRef SecretKeyReference `json:"privateKeySecretRef"`

	// AllowedNamespaces says who may create installations of this App.
	//
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// GitHubAppStatus is the observed state of a GitHubApp.
type GitHubAppStatus struct {
	// ObservedGeneration is the generation the status describes.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Slug is the App's slug on GitHub.
	//
	// +optional
	Slug string `json:"slug,omitempty"`

	// Conditions include `Ready`.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitHubApp is a GitHub App and its private key. Admin namespaces only.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=githubgate
// +kubebuilder:printcolumn:name="Slug",type=string,JSONPath=`.status.slug`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type GitHubApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitHubAppSpec   `json:"spec"`
	Status GitHubAppStatus `json:"status,omitempty"`
}

// GitHubAppList is a list of GitHubApp.
//
// +kubebuilder:object:root=true
type GitHubAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubApp `json:"items"`
}
