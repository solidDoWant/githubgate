// Package v1alpha1 contains the githubgate API types.
//
// +kubebuilder:object:generate=true
// +groupName=githubgate.soliddowant.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the githubgate API.
	GroupVersion = schema.GroupVersion{Group: "githubgate.soliddowant.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the API types in a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&GitHubApp{}, &GitHubAppList{},
		&GitHubInstallation{}, &GitHubInstallationList{},
		&GitHubAccessPolicy{}, &GitHubAccessPolicyList{},
		&GitHubUserAuthorization{}, &GitHubUserAuthorizationList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)

	return nil
}
