package operations

import (
	"strings"

	"github.com/solidDoWant/githubgate/internal/patterns"
)

// AnyOperationMatches reports whether pattern matches the id of at least one
// operation in GitHub's REST description.
func AnyOperationMatches(pattern string) bool {
	if !patterns.ValidOperation(pattern) {
		return false
	}

	matcher := patterns.SegmentsRegexp(pattern)

	for _, operation := range restCatalogue {
		if matcher.MatchString(operation.ID) {
			return true
		}
	}

	return false
}

// AnyMutationMatches reports whether pattern matches the name of at least
// one mutation in GitHub's GraphQL schema.
func AnyMutationMatches(pattern string) bool {
	if !patterns.ValidMutation(pattern) {
		return false
	}

	matcher := patterns.SegmentsRegexp(pattern)

	for name := range mutationCatalogue {
		if matcher.MatchString(name) {
			return true
		}
	}

	return false
}

// userPathParams are the only path parameters an operation callable as a
// user may have. Each names something the user owns or a record of theirs, so
// none ties the operation to a repository, organization, team, enterprise or
// App.
var userPathParams = map[string]bool{
	"account_id": true, "attestation_id": true, "codespace_name": true,
	"export_id": true, "field_id": true, "gpg_key_id": true,
	"installation_id": true, "invitation_id": true, "issue_field_id": true,
	"issue_type_id": true, "item_id": true, "key_id": true,
	"migration_id": true, "package_name": true, "package_type": true,
	"package_version_id": true, "project_number": true, "secret_name": true,
	"space_number": true, "space_resource_id": true, "ssh_signing_key_id": true,
	"subject_digest": true, "user_id": true, "username": true,
	"view_number": true, "email": true, "actor_identifier": true,
	"actor_type": true,
}

// userTemplate reports whether template is /user or below it and names only
// parameters of userPathParams.
func userTemplate(template string) bool {
	if template != "/user" && !strings.HasPrefix(template, "/user/") {
		return false
	}

	for _, segment := range strings.Split(template, "/") {
		if !strings.HasPrefix(segment, "{") {
			continue
		}

		if !userPathParams[strings.Trim(segment, "{}")] {
			return false
		}
	}

	return true
}

// UserOperation reports whether id is the exact id of an operation in
// GitHub's REST description whose path is /user or below and names no
// repository, organization, team, enterprise or App. Only those may be called
// as a user. An id that appears under several methods qualifies only if every
// one does.
func UserOperation(id string) bool {
	found := false

	for _, operation := range restCatalogue {
		if operation.ID != id {
			continue
		}

		if !userTemplate(operation.Template) {
			return false
		}

		found = true
	}

	return found
}
