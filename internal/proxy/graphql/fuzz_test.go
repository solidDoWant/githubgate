package graphql_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy/graphql"
)

// FuzzClassify checks that classification never panics, refuses only with a
// classification error, and that the rebuilt body classifies to the same
// operation, mutations and body: GitHub must run what was classified.
func FuzzClassify(f *testing.F) {
	for _, seed := range []string{
		`{"query":"{ viewer { login } }"}`,
		`{"query":"mutation M($id: ID!) { a: mergePullRequest(input: {pullRequestId: $id}) { clientMutationId } }","operationName":"M","variables":{"id":"x"}}`,
		`{"query":"query A { viewer { login } } mutation B { deleteRef(input: {}) { clientMutationId } }","operationName":"A"}`,
		`{"query":"mutation { ...F } fragment F on Mutation { deleteRef(input: {}) { clientMutationId } }"}`,
		`{"query":"{ a(s: \"\\\"\\u00e9\", b: \"\"\"block \\\"\"\" text\"\"\") }","extensions":{}}`,
		`[{"query":"{ viewer { login } }"}]`,
		`{"query":"x","query":"y"}`,
		`{"Query":"x"}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		request, err := graphql.Classify(bytes.NewReader(body))
		if err != nil {
			var refusal *graphql.Error

			require.True(t, errors.As(err, &refusal), "unexpected error %v", err)
			assert.Nil(t, request)

			return
		}

		reclassified, err := graphql.Classify(bytes.NewReader(request.Body))
		require.NoError(t, err, "rebuilt body %s", request.Body)
		assert.Equal(t, request.Kind, reclassified.Kind)
		assert.Equal(t, request.Mutations, reclassified.Mutations)
		assert.Equal(t, string(request.Body), string(reclassified.Body))
	})
}
