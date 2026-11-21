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

// accountScopedParams are the path parameters that tie an operation to an
// owner, repository, organization or enterprise instead of the user's own
// account.
var accountScopedParams = []string{"{owner}", "{repo}", "{org}", "{enterprise}"}

// UserOperation reports whether id is the exact id of an operation in
// GitHub's REST description whose path names no owner, repository,
// organization or enterprise. Only those may be called as a user. An id that
// appears under several methods qualifies only if every one does.
func UserOperation(id string) bool {
	found := false

	for _, operation := range restCatalogue {
		if operation.ID != id {
			continue
		}

		for _, param := range accountScopedParams {
			if strings.Contains(operation.Template, param) {
				return false
			}
		}

		found = true
	}

	return found
}
