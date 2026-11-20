package graphql_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/proxy/graphql"
)

// requireRefused returns an assertion that the error is a classification
// refusal with the given status and code.
func requireRefused(status int, code string) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, _ ...any) {
		var refusal *graphql.Error

		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, status, refusal.Status)
		assert.Equal(t, code, refusal.Code)
		assert.NotEmpty(t, refusal.Reason)
	}
}

func TestClassifyRefusesRequestsItCannotClassify(t *testing.T) {
	t.Parallel()

	invalid := requireRefused(http.StatusBadRequest, "invalid_request")

	tests := []struct {
		name string
		body string
		// reason, when set, must appear in the refusal's reason, for
		// shapes a later check would also refuse.
		reason string
	}{
		{name: "a batch (JSON array) is refused", body: `[{"query":"{ viewer { login } }"}]`, reason: "batches"},
		{name: "a body that isn't JSON is refused", body: `query { viewer { login } }`},
		{name: "a JSON string is refused", body: `"{ viewer { login } }"`},
		{name: "an empty body is refused", body: ``},
		{name: "a truncated object is refused", body: `{"query":"{ viewer { login } }"`},
		{name: "trailing data after the object is refused", body: `{"query":"{ viewer { login } }"} {"query":"mutation { deleteRef(input: {}) { clientMutationId } }"}`},
		{name: "an unknown key is refused", body: `{"query":"{ viewer { login } }","other":1}`},
		{name: "a key differing only in case is refused", body: `{"Query":"{ viewer { login } }"}`},
		{name: "a case variant next to the real key is refused", body: `{"query":"{ viewer { login } }","QUERY":"mutation { deleteRef(input: {}) { clientMutationId } }"}`},
		{name: "a case variant of operationName is refused", body: `{"query":"{ viewer { login } }","operationname":null}`},
		{name: "a duplicate query key is refused", body: `{"query":"{ viewer { login } }","query":"mutation { deleteRef(input: {}) { clientMutationId } }"}`},
		{name: "a duplicate operationName key is refused", body: `{"query":"query A { viewer { login } } mutation B { deleteRef(input: {}) { clientMutationId } }","operationName":"A","operationName":"B"}`},
		{name: "a duplicate key inside variables is refused", body: `{"query":"query($a: String) { viewer { login } }","variables":{"a":"x","a":"y"}}`},
		{name: "a persisted query without a query is refused", body: `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"abc"}}}`, reason: "persisted"},
		{name: "a body without a query is refused", body: `{"variables":{}}`},
		{name: "a null query is refused", body: `{"query":null}`},
		{name: "a query that isn't a string is refused", body: `{"query":{"text":"{ viewer { login } }"}}`},
		{name: "an operationName that isn't a string is refused", body: `{"query":"{ viewer { login } }","operationName":1}`},
		{name: "variables that aren't an object are refused", body: `{"query":"{ viewer { login } }","variables":[1]}`},
		{name: "extensions that aren't an object are refused", body: `{"query":"{ viewer { login } }","extensions":"x"}`},
		{name: "a document that can't be parsed is refused", body: `{"query":"mutation { deleteRef(input: {}) { clientMutationId "}`},
		{name: "a document with only fragments is refused", body: `{"query":"fragment F on Query { viewer { login } }"}`},
		{name: "several operations without operationName are refused", body: `{"query":"query A { viewer { login } } mutation B { deleteRef(input: {}) { clientMutationId } }"}`},
		{name: "an operationName naming no operation is refused", body: `{"query":"query A { viewer { login } }","operationName":"B"}`},
		{name: "an operationName naming two operations is refused", body: `{"query":"query A { viewer { login } } mutation A { deleteRef(input: {}) { clientMutationId } }","operationName":"A"}`},
		{name: "an operationName with one anonymous operation is refused", body: `{"query":"mutation { deleteRef(input: {}) { clientMutationId } }","operationName":"A"}`},
		{name: "a subscription is refused", body: `{"query":"subscription { issueUpdated { id } }"}`},
		{name: "a fragment spread at the top of a mutation is refused", body: `{"query":"mutation { ...F } fragment F on Mutation { deleteRef(input: {}) { clientMutationId } }"}`},
		{name: "an inline fragment at the top of a mutation is refused", body: `{"query":"mutation { ... on Mutation { deleteRef(input: {}) { clientMutationId } } }"}`},
		{name: "a typeless inline fragment at the top of a mutation is refused", body: `{"query":"mutation { addComment(input: {}) { clientMutationId } ... { deleteRef(input: {}) { clientMutationId } } }"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, err := graphql.Classify(strings.NewReader(test.body))
			invalid(t, err)
			assert.Nil(t, request)
			assert.Contains(t, err.Error(), test.reason)
		})
	}
}

func TestClassifyLimitsTheBodyToOneMebibyte(t *testing.T) {
	t.Parallel()

	// bodyOfSize pads the query with spaces after the document so the body
	// is exactly size bytes and still valid.
	bodyOfSize := func(size int) []byte {
		prefix := `{"query":"{ viewer { login } }`
		suffix := `"}`

		return []byte(prefix + strings.Repeat(" ", size-len(prefix)-len(suffix)) + suffix)
	}

	tests := []struct {
		name        string
		size        int
		requireErr  require.ErrorAssertionFunc
		wantRequest bool
	}{
		{name: "a body of exactly 1 MiB is classified", size: 1 << 20, wantRequest: true},
		{name: "a body one byte over 1 MiB is refused as too large", size: 1<<20 + 1, requireErr: requireRefused(http.StatusRequestEntityTooLarge, "too_large")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.requireErr == nil {
				test.requireErr = require.NoError
			}

			body := bodyOfSize(test.size)
			require.Len(t, body, test.size)

			request, err := graphql.Classify(bytes.NewReader(body))
			test.requireErr(t, err)
			assert.Equal(t, test.wantRequest, request != nil)
		})
	}
}

func TestClassifyLimitsThePrintedDocumentToOneMebibyte(t *testing.T) {
	t.Parallel()

	// A field nested depth times prints compactly as "a {\n" per level, the
	// innermost "a\n", and "}\n" per level: 6*depth+2 bytes from 3*depth+2
	// bytes of input. The document "query {\n" ... "}\n" holds chains of
	// such fields and one flat field whose name sets the exact printed size.
	const depth = 900

	chain := " a" + strings.Repeat("{a", depth) + strings.Repeat("}", depth)
	chains := 194
	chainPrinted := 6*depth + 2

	documentOfPrintedSize := func(size int) string {
		filler := size - len("query {\n") - len("}\n") - chains*chainPrinted - len("\n")

		return "query {" + strings.Repeat(chain, chains) + " " + strings.Repeat("x", filler) + "}"
	}

	tests := []struct {
		name       string
		size       int
		requireErr require.ErrorAssertionFunc
	}{
		{name: "a document printing to exactly 1 MiB is classified", size: graphql.MaxBodySize},
		{name: "a document printing to one byte over 1 MiB is refused as too large", size: graphql.MaxBodySize + 1, requireErr: requireRefused(http.StatusRequestEntityTooLarge, "too_large")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.requireErr == nil {
				test.requireErr = require.NoError
			}

			body, err := json.Marshal(map[string]string{"query": documentOfPrintedSize(test.size)})
			require.NoError(t, err)
			require.LessOrEqual(t, len(body), graphql.MaxBodySize)

			request, err := graphql.Classify(bytes.NewReader(body))
			test.requireErr(t, err)

			if request == nil {
				return
			}

			var forwarded struct {
				Query string `json:"query"`
			}

			require.NoError(t, json.Unmarshal(request.Body, &forwarded))
			assert.Len(t, forwarded.Query, test.size)
		})
	}
}

func TestClassifyPrintsNestedSelectionsWithoutIndentation(t *testing.T) {
	t.Parallel()

	// Indenting each level would print about depth² bytes.
	deep := strings.Repeat("{a", 998) + strings.Repeat("}", 998)
	body := `{"query":"query {` + strings.Repeat(" a"+deep, 7) + `}"}`

	request, err := graphql.Classify(strings.NewReader(body))
	require.NoError(t, err)
	assert.Less(t, len(request.Body), 4*len(body))
	assert.NotContains(t, string(request.Body), `\t`)
}

func TestClassifyReturnsTheOperationKindAndMutationNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantKind      graphql.Kind
		wantMutations []string
	}{
		{
			name:     "a shorthand query is a query",
			body:     `{"query":"{ viewer { login } }"}`,
			wantKind: graphql.Query,
		},
		{
			name:     "a query may use fragments at its top level",
			body:     `{"query":"query { ...F ... on Query { rateLimit { remaining } } } fragment F on Query { viewer { login } }"}`,
			wantKind: graphql.Query,
		},
		{
			name:          "a mutation returns its field names",
			body:          `{"query":"mutation { createPullRequest(input: {}) { pullRequest { url } } addComment(input: {}) { clientMutationId } }"}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"createPullRequest", "addComment"},
		},
		{
			name:          "aliases are ignored",
			body:          `{"query":"mutation { harmless: deleteRef(input: {}) { clientMutationId } }"}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"deleteRef"},
		},
		{
			name:          "a field named twice is listed once",
			body:          `{"query":"mutation { a: addComment(input: {}) { clientMutationId } b: addComment(input: {}) { clientMutationId } }"}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"addComment"},
		},
		{
			name:          "operationName selects the operation",
			body:          `{"query":"query A { viewer { login } } mutation B { mergePullRequest(input: {}) { clientMutationId } }","operationName":"B"}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"mergePullRequest"},
		},
		{
			name:     "operationName selecting a query ignores the other operation's mutations",
			body:     `{"query":"query A { viewer { login } } mutation B { mergePullRequest(input: {}) { clientMutationId } }","operationName":"A"}`,
			wantKind: graphql.Query,
		},
		{
			name:          "fragments below the top level of a mutation are allowed",
			body:          `{"query":"mutation { addComment(input: {}) { ...F } } fragment F on AddCommentPayload { clientMutationId }"}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"addComment"},
		},
		{
			name:          "null operationName, variables and extensions are accepted",
			body:          `{"query":"mutation { addComment(input: {}) { clientMutationId } }","operationName":null,"variables":null,"extensions":null}`,
			wantKind:      graphql.Mutation,
			wantMutations: []string{"addComment"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, err := graphql.Classify(strings.NewReader(test.body))
			require.NoError(t, err)
			assert.Equal(t, test.wantKind, request.Kind)
			assert.Equal(t, test.wantMutations, request.Mutations)
		})
	}
}

func TestClassifyRebuildsTheForwardedBody(t *testing.T) {
	t.Parallel()

	body := `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"abc"}},` +
		`"variables":{"id":"PR_1","nested":{"a":[1,2]}},` +
		`"operationName":"Merge",` +
		`"query":"# a comment\nquery Read { viewer { login } }\nmutation Merge($id: ID!) { m: mergePullRequest(input: {pullRequestId: $id}) { clientMutationId } }"}`

	request, err := graphql.Classify(strings.NewReader(body))
	require.NoError(t, err)

	var forwarded map[string]json.RawMessage

	require.NoError(t, json.Unmarshal(request.Body, &forwarded))
	assert.ElementsMatch(t, []string{"query", "operationName", "variables"}, keys(forwarded),
		"only the classified values are forwarded")
	assert.JSONEq(t, `"Merge"`, string(forwarded["operationName"]))
	assert.JSONEq(t, `{"id":"PR_1","nested":{"a":[1,2]}}`, string(forwarded["variables"]))

	var query string

	require.NoError(t, json.Unmarshal(forwarded["query"], &query))
	assert.NotContains(t, query, "a comment", "the query is printed from the parsed document")
	assert.Contains(t, query, "m: mergePullRequest")

	reclassified, err := graphql.Classify(bytes.NewReader(request.Body))
	require.NoError(t, err)
	assert.Equal(t, request.Kind, reclassified.Kind)
	assert.Equal(t, request.Mutations, reclassified.Mutations)
	assert.Equal(t, request.Body, reclassified.Body)
}

func TestClassifyOmitsUnsetValuesFromTheForwardedBody(t *testing.T) {
	t.Parallel()

	request, err := graphql.Classify(strings.NewReader(`{"query":"{ viewer { login } }","variables":null}`))
	require.NoError(t, err)

	var forwarded map[string]json.RawMessage

	require.NoError(t, json.Unmarshal(request.Body, &forwarded))
	assert.Equal(t, []string{"query"}, keys(forwarded))
}

func keys(object map[string]json.RawMessage) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}

	return names
}
