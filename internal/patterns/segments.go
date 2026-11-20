package patterns

import (
	"regexp"
	"strings"
)

var (
	validOperation = regexp.MustCompile(`^[a-z0-9*/-]{1,255}$`)
	validMutation  = regexp.MustCompile(`^[A-Za-z0-9*]{1,255}$`)
	invalidRefByte = regexp.MustCompile(`[\x00-\x20\x7f~^:?\[\\]`)
)

// ValidOperation reports whether pattern is a well-formed REST operation id
// pattern: lower-case letters, digits, `-`, `/` and `*`, with no empty
// segment.
func ValidOperation(pattern string) bool {
	return validOperation.MatchString(pattern) && !strings.Contains(pattern, "**") && segmentsNonEmpty(pattern)
}

// ValidMutation reports whether pattern is a well-formed GraphQL mutation
// name pattern: letters, digits and `*`.
func ValidMutation(pattern string) bool {
	return validMutation.MatchString(pattern) && !strings.Contains(pattern, "**")
}

// ValidRef reports whether pattern is a well-formed git ref pattern: it
// starts with `refs/`, has only the characters `git check-ref-format`
// allows (plus `*`), no `..` or `@{`, and no empty segment or segment
// starting or ending with `.`. A `**` must be a whole segment.
func ValidRef(pattern string) bool {
	if len(pattern) > 255 || !strings.HasPrefix(pattern, "refs/") || len(pattern) == len("refs/") {
		return false
	}

	if invalidRefByte.MatchString(pattern) || strings.Contains(pattern, "..") || strings.Contains(pattern, "@{") {
		return false
	}

	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}

		if segment != "**" && strings.Contains(segment, "**") {
			return false
		}
	}

	return true
}

func segmentsNonEmpty(pattern string) bool {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" {
			return false
		}
	}

	return true
}

// MatchSegments reports whether name matches pattern, both split on `/`. In
// a segment, `*` matches one or more characters other than `/`; a segment
// that is exactly `**` matches one or more whole segments. Matching is
// case-sensitive and takes time linear in the name.
func MatchSegments(pattern, name string) bool {
	return SegmentsRegexp(pattern).MatchString(name)
}

// SegmentsRegexp compiles a segment pattern, as MatchSegments reads it, to
// an RE2 expression, which never backtracks. Use it to match one pattern
// against many names.
func SegmentsRegexp(pattern string) *regexp.Regexp {
	var expression strings.Builder

	expression.WriteString("^")

	for index, segment := range strings.Split(pattern, "/") {
		if index > 0 {
			expression.WriteString("/")
		}

		if segment == "**" {
			expression.WriteString(`[^/]+(?:/[^/]+)*`)
			continue
		}

		for part, literal := range strings.Split(segment, "*") {
			if part > 0 {
				expression.WriteString(`[^/]+`)
			}

			expression.WriteString(regexp.QuoteMeta(literal))
		}
	}

	expression.WriteString("$")

	return regexp.MustCompile(expression.String())
}
