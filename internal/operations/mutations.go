package operations

// IsMutation reports whether name is a field of the Mutation type in GitHub's
// GraphQL schema. Any other name is unknown.
func IsMutation(name string) bool {
	_, ok := mutationCatalogue[name]

	return ok
}
