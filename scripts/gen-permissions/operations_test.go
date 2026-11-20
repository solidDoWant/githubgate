package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckTemplate(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		slash     string
		shape     string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "literals only", template: "/user", shape: "/user"},
		{name: "root", template: "/", shape: "/"},
		{name: "single-segment parameters", template: "/repos/{owner}/{repo}", shape: "/repos/{*}/{*}"},
		{name: "slash parameter last", template: "/repos/{owner}/{repo}/git/refs/{ref}", slash: "ref", shape: "/repos/{*}/{*}/git/refs/{**}"},
		{name: "slash parameter followed by literals", template: "/repos/{owner}/{repo}/commits/{ref}/status", slash: "ref", shape: "/repos/{*}/{*}/commits/{**}/status"},
		{name: "reviewed single-segment parameter", template: "/repos/{owner}/{repo}/git/trees/{tree_sha}", shape: "/repos/{*}/{*}/git/trees/{*}"},
		{name: "unclassified parameter on the review list", template: "/repos/{owner}/{repo}/heads/{head_ref}", assertErr: require.Error},
		{name: "two slash parameters", template: "/repos/{owner}/{repo}/compare/{ref}/{path}", assertErr: require.Error},
		{name: "parameter after a slash parameter", template: "/repos/{owner}/{repo}/contents/{path}/{owner}", assertErr: require.Error},
		{name: "mixed segment", template: "/repos/{owner}/{repo}.git", assertErr: require.Error},
		{name: "empty segment", template: "/repos//x", assertErr: require.Error},
		{name: "relative", template: "repos/{owner}", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertErr := test.assertErr
			if assertErr == nil {
				assertErr = require.NoError
			}

			slash, shape, err := checkTemplate(test.template)
			assertErr(t, err)

			if err == nil {
				assert.Equal(t, test.slash, slash)
				assert.Equal(t, test.shape, shape)
			}
		})
	}
}

func TestGenerateREST(t *testing.T) {
	src, err := generateREST(map[string]map[string]string{
		"/repos/{owner}/{repo}/contents/{path}": {"get": "repos/get-content", "put": "repos/create-or-update-file-contents"},
		"/user":                                 {"get": "users/get-authenticated"},
	})
	require.NoError(t, err)
	assert.Contains(t, string(src), `{Method: "GET", ID: "repos/get-content", Template: "/repos/{owner}/{repo}/contents/{path}", SlashParam: "path"}`)
	assert.Contains(t, string(src), `{Method: "PUT", ID: "repos/create-or-update-file-contents", Template: "/repos/{owner}/{repo}/contents/{path}", SlashParam: "path"}`)
	assert.Contains(t, string(src), `{Method: "GET", ID: "users/get-authenticated", Template: "/user", SlashParam: ""}`)

	_, err = generateREST(map[string]map[string]string{
		"/orgs/{org}":  {"get": "orgs/get"},
		"/orgs/{name}": {"get": "orgs/get-by-name"},
	})
	require.Error(t, err, "two templates matching the same paths")

	_, err = generateREST(map[string]map[string]string{"/user": {"get": ""}})
	require.Error(t, err, "operation without an id")

	_, err = generateREST(nil)
	require.Error(t, err)
}

func TestGenerateMutationsListsEveryField(t *testing.T) {
	src, err := generateMutations(`
type Query { viewer: String }
"""Mutations."""
type Mutation {
  "Creates a ref."
  createRef(input: String!): String
  addComment(input: String!): String
}
extend type Mutation { mergeBranch(input: String!): String }
`)
	require.NoError(t, err)
	assert.Contains(t, string(src), `"addComment":  {},`)
	assert.Contains(t, string(src), `"createRef":   {},`)
	assert.Contains(t, string(src), `"mergeBranch": {},`)
	assert.NotContains(t, string(src), `"viewer"`)

	_, err = generateMutations(`type Query { viewer: String }`)
	require.Error(t, err, "no Mutation type")

	_, err = generateMutations(`type Mutation {`)
	require.Error(t, err, "unparseable schema")
}
