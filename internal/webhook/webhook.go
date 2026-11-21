// Package webhook serves the validating admission webhook for GitHubApp,
// GitHubInstallation, GitHubUserAuthorization and GitHubAccessPolicy.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

const maxBodyBytes = 3 << 20

// Installations looks up existing GitHubInstallations.
type Installations interface {
	// GetInstallation returns the installation, or nil when it doesn't exist.
	GetInstallation(ctx context.Context, name types.NamespacedName) (*githubgatev1alpha1.GitHubInstallation, error)
}

// Handler answers AdmissionReview requests.
type Handler struct {
	adminNamespaces []string
	installations   Installations
}

// New returns a handler that admits GitHubApp and GitHubInstallation objects
// only in adminNamespaces.
func New(adminNamespaces []string, installations Installations) *Handler {
	return &Handler{adminNamespaces: adminNamespaces, installations: installations}
}

// ServeHTTP decodes an AdmissionReview, validates the object and answers with
// the verdict. Anything it can't decide is denied.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxBodyBytes))
	if err != nil {
		http.Error(writer, "cannot read the request", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(writer, "not an AdmissionReview", http.StatusBadRequest)
		return
	}

	response := &admissionv1.AdmissionResponse{UID: review.Request.UID}

	if message := h.validate(request.Context(), review.Request); message != "" {
		response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: message,
		}
	} else {
		response.Allowed = true
	}

	review.Request = nil
	review.Response = response
	review.APIVersion, review.Kind = "admission.k8s.io/v1", "AdmissionReview"

	writer.Header().Set("Content-Type", "application/json")

	// A failed write can't be reported to a client that has gone away.
	_ = json.NewEncoder(writer).Encode(review)
}

// validate returns the reason to reject the request, or "" to admit it.
func (h *Handler) validate(ctx context.Context, request *admissionv1.AdmissionRequest) string {
	if request.Operation == admissionv1.Delete {
		return ""
	}

	switch request.Kind.Kind {
	case "GitHubApp":
		return h.checkAdminNamespace(request.Namespace)
	case "GitHubInstallation":
		var installation githubgatev1alpha1.GitHubInstallation
		if err := json.Unmarshal(request.Object.Raw, &installation); err != nil {
			return "cannot decode the GitHubInstallation"
		}

		if message := h.checkAdminNamespace(request.Namespace); message != "" {
			return message
		}

		return validateCeiling(installation.Spec.Ceiling)
	case "GitHubUserAuthorization":
		var authorization githubgatev1alpha1.GitHubUserAuthorization
		if err := json.Unmarshal(request.Object.Raw, &authorization); err != nil {
			return "cannot decode the GitHubUserAuthorization"
		}

		if message := h.checkAdminNamespace(request.Namespace); message != "" {
			return message
		}

		for index, operation := range authorization.Spec.Ceiling.Operations {
			if message := model.InvalidUserOperation(string(operation)); message != "" {
				return fmt.Sprintf("spec.ceiling.operations[%d]: %s", index, message)
			}
		}

		return ""
	case "GitHubAccessPolicy":
		var policy githubgatev1alpha1.GitHubAccessPolicy
		if err := json.Unmarshal(request.Object.Raw, &policy); err != nil {
			return "cannot decode the GitHubAccessPolicy"
		}

		return h.validateRules(ctx, request.Namespace, policy.Spec.Rules)
	default:
		return fmt.Sprintf("unexpected kind %q", request.Kind.Kind)
	}
}

func (h *Handler) checkAdminNamespace(namespace string) string {
	if slices.Contains(h.adminNamespaces, namespace) {
		return ""
	}

	return fmt.Sprintf("metadata.namespace: %q is not an admin namespace; create this object in one of %v", namespace, h.adminNamespaces)
}

func validateCeiling(ceiling []githubgatev1alpha1.PermissionSet) string {
	for index, entry := range ceiling {
		if message := validatePermissions(fmt.Sprintf("spec.ceiling[%d]", index), entry.Permissions); message != "" {
			return message
		}
	}

	return ""
}

func (h *Handler) validateRules(ctx context.Context, namespace string, rules []githubgatev1alpha1.AccessRule) string {
	for index, rule := range rules {
		path := fmt.Sprintf("spec.rules[%d]", index)

		if message := validatePermissions(path, rule.Permissions); message != "" {
			return message
		}

		if field, message := model.InvalidProxyMode(rule.Proxy); field != "" {
			return path + "." + field + ": " + message
		}

		if rule.UserAuthorizationRef != nil {
			if message := validateUserRuleOperations(path, rule.Proxy); message != "" {
				return message
			}
		}

		// CEL requires exactly one reference; a user rule has no
		// installation ceiling to check here.
		if rule.InstallationRef == nil {
			continue
		}

		ref := types.NamespacedName{Namespace: rule.InstallationRef.Namespace, Name: rule.InstallationRef.Name}
		if ref.Namespace == "" {
			ref.Namespace = namespace
		}

		installation, err := h.installations.GetInstallation(ctx, ref)
		if err != nil {
			return fmt.Sprintf("%s.installationRef: cannot look up GitHubInstallation %s", path, ref)
		}

		// A reference to an object that doesn't exist yet is allowed.
		if installation == nil {
			continue
		}

		ceiling := make([]permissions.Grant, 0, len(installation.Spec.Ceiling))
		for _, entry := range installation.Spec.Ceiling {
			ceiling = append(ceiling, model.GrantOf(entry.Repositories, entry.Permissions))
		}

		if !permissions.Contained(model.GrantOf(rule.Repositories, rule.Permissions), ceiling) {
			return fmt.Sprintf("%s: the rule is outside the ceiling of GitHubInstallation %s", path, ref)
		}
	}

	return ""
}

func validateUserRuleOperations(path string, proxy *githubgatev1alpha1.ProxyMode) string {
	if proxy == nil {
		return ""
	}

	for index, operation := range proxy.AllowOperations {
		if message := model.InvalidUserOperation(string(operation)); message != "" {
			return fmt.Sprintf("%s.proxy.allowOperations[%d]: %s", path, index, message)
		}
	}

	return ""
}

func validatePermissions(path string, perms map[string]githubgatev1alpha1.Level) string {
	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		level := permissions.Level(perms[name])
		if !permissions.ValidRepositoryPermission(name, level) {
			return fmt.Sprintf("%s.permissions[%s]: %q is not a known repository permission at level %q", path, name, name, level)
		}
	}

	return ""
}
