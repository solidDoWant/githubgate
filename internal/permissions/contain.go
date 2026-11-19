package permissions

import "github.com/solidDoWant/githubgate/internal/patterns"

// Grant is a set of repository patterns with the permissions they carry. A
// policy rule and an installation ceiling entry both have this shape.
type Grant struct {
	Repositories []string
	Permissions  map[string]Level
}

// Contained reports whether every (pattern, permission, level) of rule is
// covered by a single ceiling entry: one with a pattern that contains the
// rule's pattern and a level for the permission at or above the rule's.
// An empty rule is not contained.
func Contained(rule Grant, ceiling []Grant) bool {
	if len(rule.Repositories) == 0 || len(rule.Permissions) == 0 {
		return false
	}

	for _, pattern := range rule.Repositories {
		for name, level := range rule.Permissions {
			if !covered(pattern, name, level, ceiling) {
				return false
			}
		}
	}

	return true
}

func covered(pattern, name string, level Level, ceiling []Grant) bool {
	for _, entry := range ceiling {
		have, ok := entry.Permissions[name]
		if !ok || !have.AtLeast(level) {
			continue
		}

		for _, container := range entry.Repositories {
			if patterns.Contains(container, pattern) {
				return true
			}
		}
	}

	return false
}
