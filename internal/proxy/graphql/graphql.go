// Package graphql classifies a GraphQL request body sent to the proxy: it
// refuses every shape githubgate can't classify with certainty, finds the
// operation GitHub will run and its mutation names, and rebuilds the body so
// GitHub runs exactly the document that was classified.
package graphql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"
)

// MaxBodySize is the largest GraphQL body the proxy reads, in bytes.
const MaxBodySize = 1 << 20

// maxDepth bounds the nesting of JSON values, as encoding/json does, so a
// deeply nested body can't exhaust the stack.
const maxDepth = 10000

// Kind is the kind of the operation GitHub will run.
type Kind int

const (
	// Query is a query operation.
	Query Kind = iota + 1
	// Mutation is a mutation operation.
	Mutation
)

// String returns the GraphQL keyword of the kind.
func (k Kind) String() string {
	switch k {
	case Query:
		return "query"
	case Mutation:
		return "mutation"
	default:
		return "unknown"
	}
}

// Request is a classified GraphQL request.
type Request struct {
	// Kind is the kind of the selected operation.
	Kind Kind
	// Mutations are the top-level field names of a mutation, aliases
	// ignored, each once, in document order. Empty for a query.
	Mutations []string
	// Body is the body to forward: only the classified values, with the
	// query printed from the parsed document.
	Body []byte
}

// Error is a refusal. Status and Code are the HTTP status and the error code
// the proxy answers with; Reason says why, for the caller.
type Error struct {
	Status int
	Code   string
	Reason string
}

func (e *Error) Error() string {
	return e.Reason
}

func invalid(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Reason: fmt.Sprintf(format, args...)}
}

// forwardedBody is the body githubgate sends to GitHub. extensions is never
// forwarded: githubgate doesn't classify it, so GitHub must not act on it.
type forwardedBody struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// Classify reads a GraphQL request body of at most MaxBodySize bytes and
// classifies it. Any shape it can't classify with certainty is refused with an
// *Error.
func Classify(body io.Reader) (*Request, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the GraphQL body: %w", err)
	}

	if len(data) > MaxBodySize {
		return nil, &Error{
			Status: http.StatusRequestEntityTooLarge,
			Code:   "too_large",
			Reason: fmt.Sprintf("GraphQL body is larger than %d bytes", MaxBodySize),
		}
	}

	fields, refusal := decodeTopLevel(data)
	if refusal != nil {
		return nil, refusal
	}

	document, parseErr := parser.ParseQuery(&ast.Source{Input: fields.Query})
	if parseErr != nil {
		return nil, invalid("GraphQL document can't be parsed")
	}

	operation, refusal := selectOperation(document, fields.OperationName)
	if refusal != nil {
		return nil, refusal
	}

	request := &Request{}

	switch operation.Operation {
	case ast.Query:
		request.Kind = Query
	case ast.Mutation:
		request.Kind = Mutation

		request.Mutations, refusal = mutationNames(operation)
		if refusal != nil {
			return nil, refusal
		}
	default:
		return nil, invalid("GraphQL %s operations are not supported", operation.Operation)
	}

	var printed bytes.Buffer

	formatter.NewFormatter(&printed).FormatQueryDocument(document)
	fields.Query = printed.String()

	request.Body, err = json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding the GraphQL body: %w", err)
	}

	return request, nil
}

// decodeTopLevel decodes the top-level object of a GraphQL body with exact,
// unique keys.
func decodeTopLevel(data []byte) (*forwardedBody, *Error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil {
		return nil, invalid("GraphQL body is not valid JSON")
	}

	if token != json.Delim('{') {
		return nil, invalid("GraphQL body must be one JSON object; batches are not supported")
	}

	fields := &forwardedBody{}
	seen := map[string]bool{}
	hasQuery := false

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalid("GraphQL body is not valid JSON")
		}

		key, _ := token.(string)
		if seen[key] {
			return nil, invalid("GraphQL body has the key %q more than once", key)
		}

		seen[key] = true

		value, err := decodeValue(decoder, 1)
		if err != nil {
			return nil, invalid("GraphQL body is not valid JSON: %v", err)
		}

		// Keys are compared exactly: GitHub's own decoding must not find a
		// value githubgate didn't classify.
		switch key {
		case "query":
			query, ok := value.(string)
			if !ok {
				return nil, invalid("GraphQL query must be a string")
			}

			fields.Query = query
			hasQuery = true
		case "operationName":
			name, ok := value.(string)
			if !ok && value != nil {
				return nil, invalid("GraphQL operationName must be a string")
			}

			fields.OperationName = name
		case "variables":
			variables, ok := value.(map[string]any)
			if !ok && value != nil {
				return nil, invalid("GraphQL variables must be an object")
			}

			fields.Variables = variables
		case "extensions":
			if _, ok := value.(map[string]any); !ok && value != nil {
				return nil, invalid("GraphQL extensions must be an object")
			}
		default:
			return nil, invalid("GraphQL body has the unsupported key %q", key)
		}
	}

	if _, err := decoder.Token(); err != nil {
		return nil, invalid("GraphQL body is not valid JSON")
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("GraphQL body has data after its object")
	}

	if !hasQuery {
		return nil, invalid("GraphQL body has no query; persisted queries are not supported")
	}

	return fields, nil
}

var errDuplicateKey = errors.New("an object has a key more than once")

// decodeValue reads one JSON value from decoder, refusing objects with a key
// more than once at any depth.
func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("values are nested too deeply")
	}

	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	switch token {
	case json.Delim('{'):
		object := map[string]any{}

		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}

			key, _ := keyToken.(string)
			if _, exists := object[key]; exists {
				return nil, errDuplicateKey
			}

			object[key], err = decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
		}

		if _, err := decoder.Token(); err != nil {
			return nil, err
		}

		return object, nil
	case json.Delim('['):
		array := []any{}

		for decoder.More() {
			element, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}

			array = append(array, element)
		}

		if _, err := decoder.Token(); err != nil {
			return nil, err
		}

		return array, nil
	default:
		return token, nil
	}
}

// selectOperation finds the operation GitHub will run: the only one, or the
// one operationName names.
func selectOperation(document *ast.QueryDocument, operationName string) (*ast.OperationDefinition, *Error) {
	if operationName == "" {
		if len(document.Operations) != 1 {
			return nil, invalid("GraphQL document must have exactly one operation, or an operationName naming one")
		}

		return document.Operations[0], nil
	}

	var selected *ast.OperationDefinition

	for _, operation := range document.Operations {
		if operation.Name != operationName {
			continue
		}

		if selected != nil {
			return nil, invalid("GraphQL document has more than one operation named %q", operationName)
		}

		selected = operation
	}

	if selected == nil {
		return nil, invalid("GraphQL document has no operation named %q", operationName)
	}

	return selected, nil
}

// mutationNames returns the top-level field names of a mutation. A fragment
// at the top level is refused: it would hide mutation names from the check.
func mutationNames(operation *ast.OperationDefinition) ([]string, *Error) {
	var names []string

	seen := map[string]bool{}

	for _, selection := range operation.SelectionSet {
		field, ok := selection.(*ast.Field)
		if !ok {
			return nil, invalid("GraphQL mutations must name their fields directly, not through fragments")
		}

		if !seen[field.Name] {
			seen[field.Name] = true
			names = append(names, field.Name)
		}
	}

	return names, nil
}
