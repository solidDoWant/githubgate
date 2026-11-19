package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Level is a GitHub App permission level. A higher level contains the lower ones.
//
// +kubebuilder:validation:Enum=read;write;admin
type Level string

const (
	LevelRead  Level = "read"
	LevelWrite Level = "write"
	LevelAdmin Level = "admin"
)

// Repository is a repository name under an installation's owner (not
// `owner/name`). `*` matches any run of characters.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._*-]+$`
type Repository string

// SecretKeyReference names a key of a Secret in the referring object's own namespace.
type SecretKeyReference struct {
	// Name is the Secret's name.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the Secret's data. The default depends on the field.
	//
	// +optional
	Key string `json:"key,omitempty"`
}

// ObjectReference refers to an object by namespace and name.
type ObjectReference struct {
	// Namespace defaults to the referring object's own namespace.
	//
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the object's name.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AllowedNamespaces says which namespaces may reference an object. The
// object's own namespace is always admitted.
type AllowedNamespaces struct {
	// Names lists namespaces by name.
	//
	// +optional
	// +listType=set
	Names []string `json:"names,omitempty"`

	// Selector admits every namespace whose labels match.
	//
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// PermissionSet is a set of repository patterns and the permissions given on each.
type PermissionSet struct {
	// Repositories are names or `*` globs under the installation's owner.
	//
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Repositories []Repository `json:"repositories"`

	// Permissions maps GitHub App permission names to levels. Names are
	// checked against GitHub's catalogue by the webhook and the model.
	//
	// +kubebuilder:validation:MinProperties=1
	Permissions map[string]Level `json:"permissions"`
}
