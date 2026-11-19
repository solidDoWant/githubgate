package permissions

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContained(t *testing.T) {
	ceiling := []Grant{
		{Repositories: []string{"app-*"}, Permissions: map[string]Level{"contents": LevelWrite, "issues": LevelRead}},
		{Repositories: []string{"infra"}, Permissions: map[string]Level{"contents": LevelRead, "actions": LevelWrite}},
	}

	tests := []struct {
		name string
		rule Grant
		want bool
	}{
		{"within one entry", Grant{[]string{"app-web"}, map[string]Level{"contents": LevelWrite}}, true},
		{"lower level", Grant{[]string{"app-web"}, map[string]Level{"contents": LevelRead}}, true},
		{"level above the ceiling", Grant{[]string{"infra"}, map[string]Level{"contents": LevelWrite}}, false},
		{"pattern outside", Grant{[]string{"other"}, map[string]Level{"contents": LevelRead}}, false},
		{"glob equal to ceiling glob", Grant{[]string{"APP-*"}, map[string]Level{"issues": LevelRead}}, true},
		{"wider glob", Grant{[]string{"*"}, map[string]Level{"issues": LevelRead}}, false},
		{"permission not in any entry", Grant{[]string{"app-web"}, map[string]Level{"pull_requests": LevelRead}}, false},
		{"each pair covered by a different entry", Grant{[]string{"app-web", "infra"}, map[string]Level{"contents": LevelRead}}, true},
		{"permission only on another entry's pattern", Grant{[]string{"infra"}, map[string]Level{"issues": LevelRead}}, false},
		{"one pattern uncovered", Grant{[]string{"app-web", "other"}, map[string]Level{"contents": LevelRead}}, false},
		{"one permission uncovered", Grant{[]string{"app-web"}, map[string]Level{"contents": LevelRead, "actions": LevelRead}}, false},
		{"empty patterns", Grant{nil, map[string]Level{"contents": LevelRead}}, false},
		{"empty permissions", Grant{[]string{"app-web"}, nil}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Contained(tt.rule, ceiling))
		})
	}
}

func TestContainedStarCeiling(t *testing.T) {
	ceiling := []Grant{{Repositories: []string{"*"}, Permissions: map[string]Level{"contents": LevelRead}}}
	assert.True(t, Contained(Grant{[]string{"a*b", "x"}, map[string]Level{"contents": LevelRead}}, ceiling))
	assert.False(t, Contained(Grant{[]string{"x"}, map[string]Level{"contents": LevelRead}}, nil))
}
