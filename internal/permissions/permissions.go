// Package permissions holds GitHub's App permission catalogue (generated) and
// the ordering and containment rules for permission levels.
package permissions

//go:generate go run ../../scripts/gen-permissions

// Level is a permission level. Levels order read < write < admin.
type Level string

// The permission levels.
const (
	LevelRead  Level = "read"
	LevelWrite Level = "write"
	LevelAdmin Level = "admin"
)

// Scope says what a permission applies to.
type Scope string

// The permission scopes. Only repository permissions can be limited per repository.
const (
	ScopeRepository   Scope = "repository"
	ScopeOrganization Scope = "organization"
	ScopeAccount      Scope = "account"
)

// Permission is one catalogue entry.
type Permission struct {
	Name   string
	Scope  Scope
	Levels []Level
}

// Rank returns the position of a level in the order, or 0 for an unknown level.
func (l Level) Rank() int {
	switch l {
	case LevelRead:
		return 1
	case LevelWrite:
		return 2
	case LevelAdmin:
		return 3
	}

	return 0
}

// AtLeast reports whether l is a known level that is not below other.
func (l Level) AtLeast(other Level) bool {
	return l.Rank() > 0 && other.Rank() > 0 && l.Rank() >= other.Rank()
}

// Lookup returns the catalogue entry for a permission name (exact, lower-case).
func Lookup(name string) (Permission, bool) {
	permission, ok := catalogue[name]
	return permission, ok
}

// Names returns every catalogue name.
func Names() []string {
	names := make([]string, 0, len(catalogue))
	for name := range catalogue {
		names = append(names, name)
	}

	return names
}

// Allows reports whether the permission exists and offers the level.
func (p Permission) Allows(level Level) bool {
	for _, allowed := range p.Levels {
		if allowed == level {
			return true
		}
	}

	return false
}

// Valid reports whether name is a known permission and level one of its levels.
func Valid(name string, level Level) bool {
	permission, ok := Lookup(name)
	return ok && permission.Allows(level)
}

// ValidRepositoryPermission reports whether Valid holds and the permission is a
// repository permission, the only kind a rule or ceiling may name.
func ValidRepositoryPermission(name string, level Level) bool {
	permission, ok := Lookup(name)
	return ok && permission.Scope == ScopeRepository && permission.Allows(level)
}
