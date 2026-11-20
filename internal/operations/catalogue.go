package operations

import "github.com/solidDoWant/githubgate/internal/patterns"

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
