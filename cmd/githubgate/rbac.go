package main

// The cluster-wide permissions of the server. Secrets and leases are granted
// by namespaced Roles in the chart, never cluster-wide.

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=githubgate.soliddowant.dev,resources=githubapps;githubinstallations;githubaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=githubgate.soliddowant.dev,resources=githubapps/status;githubinstallations/status;githubaccesspolicies/status,verbs=get;update;patch
