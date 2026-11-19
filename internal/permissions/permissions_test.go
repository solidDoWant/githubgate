package permissions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValid(t *testing.T) {
	tests := []struct {
		name  string
		level Level
		want  bool
	}{
		{"contents", LevelRead, true},
		{"contents", LevelWrite, true},
		{"contents", LevelAdmin, false},
		{"repository_projects", LevelAdmin, true},
		{"workflows", LevelWrite, true},
		{"workflows", LevelRead, false},
		{"contents", "", false},
		{"contents", "Read", false},
		{"nonsense", LevelRead, false},
		{"Contents", LevelRead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.level), func(t *testing.T) {
			assert.Equal(t, tt.want, Valid(tt.name, tt.level))
		})
	}
}

func TestValidRepositoryPermission(t *testing.T) {
	assert.True(t, ValidRepositoryPermission("contents", LevelRead))
	assert.False(t, ValidRepositoryPermission("members", LevelRead))
	assert.False(t, ValidRepositoryPermission("organization_secrets", LevelWrite))
	assert.False(t, ValidRepositoryPermission("profile", LevelWrite))
	assert.False(t, ValidRepositoryPermission("contents", LevelAdmin))
}

func TestCataloguePlacesEveryPermission(t *testing.T) {
	require.NotEmpty(t, Names())

	for _, name := range Names() {
		permission, ok := Lookup(name)
		require.True(t, ok)
		assert.NotEmpty(t, permission.Levels, name)
		assert.Contains(t, []Scope{ScopeRepository, ScopeOrganization, ScopeAccount}, permission.Scope, name)
	}
}

func TestLevelOrder(t *testing.T) {
	assert.True(t, LevelWrite.AtLeast(LevelRead))
	assert.True(t, LevelWrite.AtLeast(LevelWrite))
	assert.False(t, LevelRead.AtLeast(LevelWrite))
	assert.True(t, LevelAdmin.AtLeast(LevelWrite))
	assert.False(t, LevelWrite.AtLeast(LevelAdmin))
	assert.False(t, Level("bogus").AtLeast(LevelRead))
	assert.False(t, LevelAdmin.AtLeast("bogus"))
}
