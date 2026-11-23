package proxy

import (
	"net/url"
	"slices"
	"sort"
	"strings"
)

const (
	// maxLoggedPath bounds the path a refused request's audit line holds,
	// so a client can't make one line arbitrarily large.
	maxLoggedPath = 512
	// maxLoggedQueryNames bounds the query parameter names it holds.
	maxLoggedQueryNames = 20
	// maxLoggedQueryName bounds the length of each query parameter name it
	// holds, so the names together stay under a fixed size.
	maxLoggedQueryName = 64
	// redacted replaces a path segment or parameter name that looks like a
	// credential.
	redacted = "REDACTED"
)

// credentialMarkers are the prefixes of GitHub's token formats and of a
// JSON Web Token (ServiceAccount tokens and App JWTs). A logged segment
// that holds one is replaced, so a client that put a credential in a path
// or a parameter name never has it written to the log.
var credentialMarkers = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "eyJ"}

// refusedTarget returns what a refused request's audit line says about its
// target: the escaped path with every credential-like segment replaced and
// cut to maxLoggedPath bytes, and the sorted names of its query parameters.
// Query values are never returned.
func refusedTarget(requestURI string) (string, []string) {
	rawPath, rawQuery, _ := strings.Cut(requestURI, "?")

	segments := strings.Split(rawPath, "/")
	for index, segment := range segments {
		if looksLikeCredential(segment) {
			segments[index] = redacted
		}
	}

	path := strings.Join(segments, "/")
	if len(path) > maxLoggedPath {
		path = path[:maxLoggedPath]
	}

	// An unparseable query still yields the parameters before the error.
	query, _ := url.ParseQuery(rawQuery)

	names := make([]string, 0, len(query))
	for name := range query {
		if looksLikeCredential(name) {
			name = redacted
		} else if len(name) > maxLoggedQueryName {
			name = name[:maxLoggedQueryName]
		}

		names = append(names, name)
	}

	sort.Strings(names)
	names = slices.Compact(names)

	if len(names) > maxLoggedQueryNames {
		names = names[:maxLoggedQueryNames]
	}

	return path, names
}

// looksLikeCredential reports whether value, escaped or not, holds one of
// the credentialMarkers.
func looksLikeCredential(value string) bool {
	candidates := []string{value}
	if unescaped, err := url.PathUnescape(value); err == nil {
		candidates = append(candidates, unescaped)
	}

	for _, candidate := range candidates {
		for _, marker := range credentialMarkers {
			if strings.Contains(candidate, marker) {
				return true
			}
		}
	}

	return false
}
