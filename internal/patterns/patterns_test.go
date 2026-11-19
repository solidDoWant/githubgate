package patterns

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContains(t *testing.T) {
	tests := []struct {
		name      string
		container string
		pattern   string
		want      bool
	}{
		{"star contains literal", "*", "repo", true},
		{"star contains glob", "*", "a*b", true},
		{"star contains star", "*", "*", true},
		{"literal in matching glob", "app-*", "app-web", true},
		{"literal in glob with middle star", "a*c", "abc", true},
		{"literal not matching glob", "app-*", "web-app", false},
		{"literal equals literal", "repo", "repo", true},
		{"different literals", "repo", "other", false},
		{"glob equals itself", "app-*", "app-*", true},
		{"narrower glob is not contained by glob", "app-*", "app-web-*", false},
		{"glob not contained by literal", "repo", "re*", false},
		{"glob not contained by other glob with middle star", "a*c", "a*bc", false},
		{"case-insensitive literal", "App-*", "APP-Web", true},
		{"case-insensitive equal globs", "App-*", "aPP-*", true},
		{"case-insensitive star literal", "*", "ANY", true},
		{"literal does not contain star", "*x", "*", false},
		{"glob with prefix and suffix overlap", "ab*ba", "aba", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Contains(tt.container, tt.pattern))
		})
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"a*", "abc", true},
		{"*c", "abc", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXcYb", false},
		{"ab*ba", "aba", false},
		{"ab*ba", "abba", true},
		{"repo", "REPO", true},
		{"repo", "repo2", false},
		{"A*", "abc", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Match(tt.pattern, tt.name))
		})
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"repo", true},
		{"Re.po_-1*", true},
		{"*", true},
		{strings.Repeat("a", 100), true},
		{strings.Repeat("a", 101), false},
		{"", false},
		{"a/b", false},
		{"a b", false},
		{"a?", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			assert.Equal(t, tt.want, Valid(tt.pattern))
		})
	}
}
