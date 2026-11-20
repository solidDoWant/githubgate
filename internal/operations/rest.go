// Package operations classifies GitHub API requests: it matches a REST
// request to its operation in GitHub's REST description, knows the GraphQL
// mutations of GitHub's schema, and says which of them write git refs.
package operations

import (
	"net/http"
	"strings"
)

// restOperation is one method and path template of the REST description.
type restOperation struct {
	Method   string
	ID       string
	Template string
	// SlashParam names the one parameter whose value may contain "/"; it
	// matches one or more segments and only literals follow it.
	SlashParam string
}

// RESTMatch is the operation a REST request was matched to.
type RESTMatch struct {
	// ID is GitHub's operation id, for example "repos/create-release".
	ID string
	// Template is the path template, for example "/repos/{owner}/{repo}/releases".
	Template string
	// Params holds each path parameter's value as it appeared in the path
	// (still percent-encoded); a slash parameter's value keeps its "/".
	Params map[string]string
}

// Owner returns the {owner} path parameter and whether the template names it.
func (m RESTMatch) Owner() (string, bool) {
	value, ok := m.Params["owner"]

	return value, ok
}

// Repo returns the {repo} path parameter and whether the template names it.
func (m RESTMatch) Repo() (string, bool) {
	value, ok := m.Params["repo"]

	return value, ok
}

type segment struct {
	literal string
	param   string
	slash   bool
}

type compiledOperation struct {
	restOperation
	segments []segment
	literals int
}

var restByMethod = compileREST(restCatalogue)

func compileREST(catalogue []restOperation) map[string][]compiledOperation {
	byMethod := map[string][]compiledOperation{}

	for _, op := range catalogue {
		compiled := compiledOperation{restOperation: op}

		for _, part := range splitPath(op.Template) {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				name := part[1 : len(part)-1]
				compiled.segments = append(compiled.segments, segment{param: name, slash: name == op.SlashParam})

				continue
			}

			compiled.segments = append(compiled.segments, segment{literal: part})
			compiled.literals++
		}

		byMethod[op.Method] = append(byMethod[op.Method], compiled)
	}

	return byMethod
}

// splitPath splits an absolute path into its segments; "/" has none.
func splitPath(path string) []string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "/")
}

// MatchREST returns the REST operation for a method and an escaped request
// path (without the query), or false when no operation of GitHub's REST
// description matches: the request is then unknown.
//
// HEAD is matched as the GET of the same path. Literal segments match
// before parameters: of the templates that match, the one with the most
// literal segments wins, and on a tie the one whose first literal comes
// earlier. A path with an empty segment matches nothing.
func MatchREST(method, path string) (RESTMatch, bool) {
	if method == http.MethodHead {
		method = http.MethodGet
	}

	if !strings.HasPrefix(path, "/") {
		return RESTMatch{}, false
	}

	parts := splitPath(path)
	for _, part := range parts {
		if part == "" {
			return RESTMatch{}, false
		}
	}

	var best *compiledOperation

	for index := range restByMethod[method] {
		candidate := &restByMethod[method][index]
		if !candidate.matches(parts) {
			continue
		}

		if best == nil || candidate.beats(best) {
			best = candidate
		}
	}

	if best == nil {
		return RESTMatch{}, false
	}

	return RESTMatch{ID: best.ID, Template: best.Template, Params: best.params(parts)}, true
}

// span returns where the template's segment index lies in parts: a slash
// parameter covers every part that the literals after it leave over.
func (op *compiledOperation) span(index, parts int) (int, int) {
	extra := parts - len(op.segments)

	for before := range index {
		if op.segments[before].slash {
			return index + extra, index + extra + 1
		}
	}

	if op.segments[index].slash {
		return index, index + extra + 1
	}

	return index, index + 1
}

func (op *compiledOperation) matches(parts []string) bool {
	slash := op.SlashParam != ""

	switch {
	case !slash && len(parts) != len(op.segments):
		return false
	case slash && len(parts) < len(op.segments):
		return false
	}

	for index, seg := range op.segments {
		if seg.literal == "" {
			continue
		}

		start, _ := op.span(index, len(parts))
		if parts[start] != seg.literal {
			return false
		}
	}

	return true
}

func (op *compiledOperation) params(parts []string) map[string]string {
	params := map[string]string{}

	for index, seg := range op.segments {
		if seg.param == "" {
			continue
		}

		start, end := op.span(index, len(parts))
		params[seg.param] = strings.Join(parts[start:end], "/")
	}

	return params
}

// beats reports whether op is a more literal match than other.
func (op *compiledOperation) beats(other *compiledOperation) bool {
	if op.literals != other.literals {
		return op.literals > other.literals
	}

	for index := range min(len(op.segments), len(other.segments)) {
		mine, theirs := op.segments[index].literal != "", other.segments[index].literal != ""
		if mine != theirs {
			return mine
		}
	}

	return len(op.segments) > len(other.segments)
}
