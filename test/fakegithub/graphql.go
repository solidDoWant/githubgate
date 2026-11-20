package fakegithub

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// PullRequest is a pull request created through GraphQL.
type PullRequest struct {
	Number int
	Title  string
	Body   string
	Head   string
	Base   string
}

// PullRequests returns the pull requests created in a repository.
func (s *Server) PullRequests(owner, name string) []PullRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]PullRequest(nil), s.pulls[owner+"/"+name]...)
}

func repositoryNodeID(owner, name string) string {
	return "R_" + hex.EncodeToString([]byte(owner+"/"+name))
}

func parseNodeID(id string) (string, string, bool) {
	raw, err := hex.DecodeString(strings.TrimPrefix(id, "R_"))
	if err != nil || !strings.HasPrefix(id, "R_") {
		return "", "", false
	}

	owner, name, ok := strings.Cut(string(raw), "/")

	return owner, name, ok
}

type graphqlError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
}

// handleGraphQL answers the documents the scenarios send: a `repository`
// query and the `createPullRequest` mutation. GitHub answers 200 with an
// `errors` list for refused fields, and `FORBIDDEN` when the token lacks the
// permission:
// https://docs.github.com/en/graphql/guides/forming-calls-with-graphql#communicating-with-graphql
func (s *Server) handleGraphQL(writer http.ResponseWriter, request *http.Request, token tokenContext) {
	var body struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if !readBody(writer, request, &body) {
		return
	}

	doc, parseErr := parser.ParseQuery(&ast.Source{Input: body.Query})
	if parseErr != nil {
		writeJSON(writer, http.StatusOK, map[string]any{"errors": []graphqlError{{Message: "Parse error: " + parseErr.Error()}}})

		return
	}

	var operation *ast.OperationDefinition

	for _, candidate := range doc.Operations {
		if body.OperationName == "" || candidate.Name == body.OperationName {
			operation = candidate

			break
		}
	}

	if operation == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"errors": []graphqlError{{Message: "A query attempted to access a non-existent operation"}}})

		return
	}

	data := map[string]any{}

	var errs []graphqlError

	for _, selection := range operation.SelectionSet {
		field, ok := selection.(*ast.Field)
		if !ok {
			errs = append(errs, graphqlError{Message: "fragments are not supported by the fake"})

			continue
		}

		value, err := s.resolveField(operation.Operation, field, body.Variables, token)
		if err != nil {
			errs = append(errs, *err)

			continue
		}

		data[field.Alias] = value
	}

	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}

	writeJSON(writer, http.StatusOK, out)
}

func (s *Server) resolveField(kind ast.Operation, field *ast.Field, vars map[string]any, token tokenContext) (any, *graphqlError) {
	args := map[string]any{}
	for _, arg := range field.Arguments {
		args[arg.Name] = valueOf(arg.Value, vars)
	}

	switch {
	case kind == ast.Query && field.Name == "repository":
		owner, _ := args["owner"].(string)
		name, _ := args["name"].(string)

		if !token.hasRepo(owner, name) {
			return nil, &graphqlError{Type: "NOT_FOUND", Message: fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name)}
		}

		return repositoryFields(field, owner, name)
	case kind == ast.Mutation && field.Name == "createPullRequest":
		return s.createPullRequest(field, args, token)
	}

	return nil, &graphqlError{Type: "undefinedField", Message: fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.Name, strings.ToUpper(string(kind[:1]))+string(kind[1:]))}
}

func valueOf(value *ast.Value, vars map[string]any) any {
	if value == nil {
		return nil
	}

	resolved, err := value.Value(vars)
	if err != nil {
		return nil
	}

	return resolved
}

func repositoryFields(field *ast.Field, owner, name string) (any, *graphqlError) {
	out := map[string]any{}

	for _, selection := range field.SelectionSet {
		sub, ok := selection.(*ast.Field)
		if !ok {
			return nil, &graphqlError{Message: "fragments are not supported by the fake"}
		}

		switch sub.Name {
		case "id":
			out[sub.Alias] = repositoryNodeID(owner, name)
		case "name":
			out[sub.Alias] = name
		case "nameWithOwner":
			out[sub.Alias] = owner + "/" + name
		default:
			return nil, &graphqlError{Type: "undefinedField", Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Repository'", sub.Name)}
		}
	}

	return out, nil
}

func stringOf(m map[string]any, key string) string {
	value, _ := m[key].(string)

	return value
}

func (s *Server) createPullRequest(field *ast.Field, args map[string]any, token tokenContext) (any, *graphqlError) {
	input, _ := args["input"].(map[string]any)
	owner, name, ok := parseNodeID(stringOf(input, "repositoryId"))

	if !ok || !token.hasRepo(owner, name) || !token.hasPermission("pull_requests", "write") {
		return nil, &graphqlError{Type: "FORBIDDEN", Message: "Resource not accessible by integration"}
	}

	head, base, title := stringOf(input, "headRefName"), stringOf(input, "baseRefName"), stringOf(input, "title")
	if head == "" || base == "" || title == "" {
		return nil, &graphqlError{Type: "INVALID", Message: "Variable $input of type CreatePullRequestInput! was provided invalid value"}
	}

	dir := s.repoDir(owner, name)
	for _, ref := range []string{head, base} {
		if _, err := runGit(dir, nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+ref); err != nil {
			return nil, &graphqlError{Type: "UNPROCESSABLE", Message: "Head sha can't be blank, Base sha can't be blank, No commits between " + base + " and " + head}
		}
	}

	s.mu.Lock()
	key := owner + "/" + name
	number := len(s.pulls[key]) + 1
	s.pulls[key] = append(s.pulls[key], PullRequest{Number: number, Title: title, Body: stringOf(input, "body"), Head: head, Base: base})
	s.mu.Unlock()

	pull := map[string]any{"id": fmt.Sprintf("PR_%s_%d", repositoryNodeID(owner, name), number), "number": number, "url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, name, number)}

	return map[string]any{"pullRequest": selectFields(field, "pullRequest", pull)}, nil
}

// selectFields keeps the fields the document selects under sub.
func selectFields(field *ast.Field, sub string, all map[string]any) map[string]any {
	out := map[string]any{}

	for _, selection := range field.SelectionSet {
		if child, ok := selection.(*ast.Field); ok && child.Name == sub {
			for _, leaf := range child.SelectionSet {
				if leafField, ok := leaf.(*ast.Field); ok {
					out[leafField.Alias] = all[leafField.Name]
				}
			}
		}
	}

	return out
}
