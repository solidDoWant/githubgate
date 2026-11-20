// Package patterns matches GitHub repository names against patterns where `*`
// matches any run of characters, and decides pattern containment. It also
// matches `/`-separated names (REST operation ids, GraphQL mutation names and
// git refs) against segment patterns.
package patterns

import (
	"regexp"
	"strings"
)

var valid = regexp.MustCompile(`^[A-Za-z0-9._*-]{1,100}$`)

// Valid reports whether pattern is a well-formed repository pattern.
func Valid(pattern string) bool {
	return valid.MatchString(pattern)
}

// Match reports whether name matches pattern. Both compare lower-cased.
func Match(pattern, name string) bool {
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)

	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}

	if !strings.HasPrefix(name, parts[0]) {
		return false
	}

	name = name[len(parts[0]):]

	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(name, part)
		if index < 0 {
			return false
		}

		name = name[index+len(part):]
	}

	return strings.HasSuffix(name, last)
}

// Contains reports whether every name p matches is matched by c. It is
// conservative: true when c is "*", when p has no "*" and matches c, or when
// p equals c (case-insensitively); otherwise false.
func Contains(c, p string) bool {
	c, p = strings.ToLower(c), strings.ToLower(p)
	if c == "*" || p == c {
		return true
	}

	return !strings.Contains(p, "*") && Match(c, p)
}
