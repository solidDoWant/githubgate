package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OperationName is the exact id of a GitHub REST operation, such as
// `users/create-ssh-signing-key-for-authenticated-user`. It has no glob.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[a-z0-9/-]+$`
type OperationName string

// UserCeiling is the most any policy may call as the authorized user.
type UserCeiling struct {
	// Operations are exact GitHub REST operation ids. The webhook accepts
	// only operations whose path names no owner, repository, organization or
	// enterprise. An empty list allows nothing. At most 256.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Operations []OperationName `json:"operations,omitempty"`
}

// GitHubUserAuthorizationSpec is the desired state of a GitHubUserAuthorization.
type GitHubUserAuthorizationSpec struct {
	// AppRef is the GitHubApp the user authorizes. The App needs a client ID.
	AppRef ObjectReference `json:"appRef"`

	// Login is the GitHub user who must authorize the App. A token of any
	// other user is discarded. It is compared case-insensitively.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`
	Login string `json:"login"`

	// Ceiling is the most any policy may call as this user.
	//
	// +optional
	Ceiling UserCeiling `json:"ceiling,omitempty"`

	// AllowedNamespaces says who may reference this authorization from a policy.
	//
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// DeviceAuthorizationStatus is a device flow in progress.
type DeviceAuthorizationStatus struct {
	// UserCode is the code the user enters at the verification URI.
	UserCode string `json:"userCode"`

	// VerificationURI is where the user enters the code.
	VerificationURI string `json:"verificationURI"`

	// ExpiresAt is when the user code stops working.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// Request is the value of the authorize annotation this flow answers.
	Request string `json:"request"`
}

// GitHubUserAuthorizationStatus is the observed state of a GitHubUserAuthorization.
type GitHubUserAuthorizationStatus struct {
	// ObservedGeneration is the generation the status describes.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Login is the login the stored token belongs to.
	//
	// +optional
	Login string `json:"login,omitempty"`

	// RefreshTokenExpiresAt is when the stored refresh token expires.
	//
	// +optional
	RefreshTokenExpiresAt *metav1.Time `json:"refreshTokenExpiresAt,omitempty"`

	// AccessTokenExpiresAt is when the stored access token expires.
	//
	// +optional
	AccessTokenExpiresAt *metav1.Time `json:"accessTokenExpiresAt,omitempty"`

	// LastRefreshTime is when the tokens were last refreshed.
	//
	// +optional
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`

	// RefreshRequestedAt is when a replica last saw GitHub refuse the
	// stored access token. The leader refreshes at once when it is later
	// than LastRefreshTime.
	//
	// +optional
	RefreshRequestedAt *metav1.Time `json:"refreshRequestedAt,omitempty"`

	// LastAuthorizeRequest is the last authorize annotation value that was
	// handled. A flow starts only for a different value.
	//
	// +optional
	LastAuthorizeRequest string `json:"lastAuthorizeRequest,omitempty"`

	// Authorization is set only while a device flow is in progress.
	//
	// +optional
	Authorization *DeviceAuthorizationStatus `json:"authorization,omitempty"`

	// Conditions include `Ready` and `AuthorizationRequired`.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitHubUserAuthorization is a GitHub App authorized as one GitHub user.
// Admin namespaces only.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=githubgate
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 242",message="metadata.name: at most 242 characters, so the token Secret's name fits"
// +kubebuilder:printcolumn:name="Login",type=string,JSONPath=`.spec.login`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type GitHubUserAuthorization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitHubUserAuthorizationSpec   `json:"spec"`
	Status GitHubUserAuthorizationStatus `json:"status,omitempty"`
}

// GitHubUserAuthorizationList is a list of GitHubUserAuthorization.
//
// +kubebuilder:object:root=true
type GitHubUserAuthorizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubUserAuthorization `json:"items"`
}
