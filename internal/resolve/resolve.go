// Package resolve decides a token-mode request: which installation, which
// repositories and which permissions a caller gets. It does no I/O; it reads
// one snapshot, so one decision never mixes two configuration versions.
package resolve

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/patterns"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

// MaxRepositories is the most repositories GitHub accepts in one token.
const MaxRepositories = 500

// Outcome is the kind of decision. Every outcome other than OutcomeAllowed
// is a refusal and is also the error reason the caller sees.
type Outcome string

// The outcomes.
const (
	OutcomeAllowed               Outcome = "allowed"
	OutcomeInvalidRequest        Outcome = "invalid_request"
	OutcomeDenied                Outcome = "denied"
	OutcomeAmbiguousInstallation Outcome = "ambiguous_installation"
	OutcomeUnavailable           Outcome = "unavailable"
)

// Status returns the HTTP status for the outcome. Anything unknown is a
// denial.
func (o Outcome) Status() int {
	switch o {
	case OutcomeAllowed:
		return http.StatusOK
	case OutcomeInvalidRequest:
		return http.StatusBadRequest
	case OutcomeAmbiguousInstallation:
		return http.StatusConflict
	case OutcomeUnavailable:
		return http.StatusServiceUnavailable
	}

	return http.StatusForbidden
}

// Identity is the authenticated caller.
type Identity struct {
	Namespace      string
	ServiceAccount string
}

// Request is what the caller asks for. Every field is optional.
type Request struct {
	// Installation names a GitHubInstallation as `namespace/name`.
	Installation string
	// Owner is the login the repositories belong to.
	Owner        string
	Repositories []string
	Permissions  map[string]permissions.Level
}

// Decision is the result of resolving a request. Installation, Repositories,
// Permissions and Rules are set only when Outcome is OutcomeAllowed, except
// that Installation is also set for OutcomeUnavailable.
type Decision struct {
	Outcome Outcome
	// Message explains a refusal. It never contains a credential.
	Message      string
	Installation *model.Installation
	Repositories []string
	Permissions  map[string]permissions.Level
	// Rules are the rules that gave the token its access, for audit.
	Rules []*model.Rule
}

// Allowed reports whether a token may be minted.
func (d Decision) Allowed() bool { return d.Outcome == OutcomeAllowed }

// View is what resolution reads from a snapshot.
type View interface {
	// Installation returns a GitHubInstallation in an admin namespace.
	Installation(name types.NamespacedName) (*model.Installation, bool)
	// Rules returns the accepted rules that name a ServiceAccount, from the
	// policies in the ServiceAccount's own namespace.
	Rules(namespace, serviceAccount string) []*model.Rule
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// Resolve decides a request. It fails closed: anything it can't decide is
// refused, and it never widens the repositories or permissions asked for.
func Resolve(caller Identity, request Request, view View) Decision {
	who := fmt.Sprintf("ServiceAccount %q in namespace %q", caller.ServiceAccount, caller.Namespace)

	repositories, refusal := validRepositories(request.Repositories)
	if refusal != "" {
		return refuse(OutcomeInvalidRequest, who, refusal)
	}

	if refusal := validPermissions(request.Permissions); refusal != "" {
		return refuse(OutcomeInvalidRequest, who, refusal)
	}

	byInstallation := map[types.NamespacedName][]*model.Rule{}

	for _, rule := range view.Rules(caller.Namespace, caller.ServiceAccount) {
		if rule.Installation == nil {
			continue
		}

		name := rule.Installation.NamespacedName
		byInstallation[name] = append(byInstallation[name], rule)
	}

	if request.Installation != "" {
		return resolveNamed(who, request, repositories, byInstallation, view)
	}

	names := make([]types.NamespacedName, 0, len(byInstallation))

	for name, rules := range byInstallation {
		if request.Owner == "" || strings.EqualFold(rules[0].Installation.Owner, request.Owner) {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		if request.Owner != "" {
			return refuse(OutcomeDenied, who, fmt.Sprintf("no accepted token rule for owner %q", request.Owner))
		}

		return refuse(OutcomeDenied, who, "not named by any accepted token rule")
	}

	sort.Slice(names, func(i, j int) bool { return names[i].String() < names[j].String() })

	var (
		allowed []Decision
		refused []Decision
	)

	for _, name := range names {
		decision := grant(who, byInstallation[name], repositories, request.Permissions)
		if decision.Allowed() {
			allowed = append(allowed, decision)
		} else {
			refused = append(refused, decision)
		}
	}

	switch len(allowed) {
	case 0:
		return refused[0]
	case 1:
		return ready(who, allowed[0])
	}

	choices := make([]string, 0, len(allowed))
	for _, decision := range allowed {
		choices = append(choices, decision.Installation.String())
	}

	return refuse(OutcomeAmbiguousInstallation, who, fmt.Sprintf(
		"more than one installation covers this request (%s): name the installation", strings.Join(choices, ", ")))
}

func resolveNamed(
	who string,
	request Request,
	repositories []string,
	byInstallation map[types.NamespacedName][]*model.Rule,
	view View,
) Decision {
	namespace, name, ok := strings.Cut(request.Installation, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return refuse(OutcomeInvalidRequest, who, fmt.Sprintf("installation %q is not of the form namespace/name", request.Installation))
	}

	key := types.NamespacedName{Namespace: namespace, Name: name}

	if installation, found := view.Installation(key); found && request.Owner != "" &&
		!strings.EqualFold(installation.Owner, request.Owner) {
		return refuse(OutcomeInvalidRequest, who, fmt.Sprintf(
			"installation %s is for owner %q, not %q", key, installation.Owner, request.Owner))
	}

	rules := byInstallation[key]
	if len(rules) == 0 {
		return refuse(OutcomeDenied, who, fmt.Sprintf("no accepted token rule for installation %s", key))
	}

	return ready(who, grant(who, rules, repositories, request.Permissions))
}

// ready turns an allowed decision through an installation that can't serve
// into a refusal.
func ready(who string, decision Decision) Decision {
	if !decision.Allowed() || decision.Installation.Ready {
		return decision
	}

	refusal := refuse(OutcomeUnavailable, who,
		fmt.Sprintf("installation %s is not ready: %s", decision.Installation, decision.Installation.Reason))
	refusal.Installation = decision.Installation

	return refusal
}

// grant resolves a request against the caller's rules for one installation.
func grant(who string, rules []*model.Rule, repositories []string, requested map[string]permissions.Level) Decision {
	installation := rules[0].Installation

	if len(repositories) == 0 {
		repositories = exactRepositories(rules)
		if len(repositories) == 0 {
			return refuse(OutcomeInvalidRequest, who, fmt.Sprintf(
				"name the repositories: your rules for installation %s only use patterns", installation))
		}
	}

	if len(repositories) > MaxRepositories {
		return refuse(OutcomeInvalidRequest, who, fmt.Sprintf(
			"%d repositories requested; at most %d are allowed in one token", len(repositories), MaxRepositories))
	}

	matched := []*model.Rule{}
	given := make([]map[string]permissions.Level, len(repositories))

	for index, repository := range repositories {
		given[index] = map[string]permissions.Level{}

		for _, rule := range rules {
			if !slices.ContainsFunc(rule.Repositories, func(pattern string) bool { return patterns.Match(pattern, repository) }) {
				continue
			}

			if !slices.Contains(matched, rule) {
				matched = append(matched, rule)
			}

			for name, level := range rule.Permissions {
				if level.Rank() > given[index][name].Rank() {
					given[index][name] = level
				}
			}
		}

		if len(given[index]) == 0 {
			return refuse(OutcomeDenied, who, fmt.Sprintf(
				"repository %q is not covered through installation %s", repository, installation))
		}
	}

	var result map[string]permissions.Level

	if len(requested) > 0 {
		for _, name := range sortedKeys(requested) {
			for index, repository := range repositories {
				if !given[index][name].AtLeast(requested[name]) {
					return refuse(OutcomeDenied, who, fmt.Sprintf(
						"permission %s: %s on repository %q is not covered through installation %s",
						name, requested[name], repository, installation))
				}
			}
		}

		result = maps(requested)
	} else {
		result = lowestCommon(given)
		if len(result) == 0 {
			return refuse(OutcomeDenied, who, fmt.Sprintf(
				"repositories %s share no permission through installation %s", quoted(repositories), installation))
		}
	}

	return Decision{
		Outcome:      OutcomeAllowed,
		Installation: installation,
		Repositories: repositories,
		Permissions:  result,
		Rules:        matched,
	}
}

// exactRepositories returns the names (without `*`) the rules give, without
// case-insensitive duplicates, sorted.
func exactRepositories(rules []*model.Rule) []string {
	seen := map[string]bool{}
	names := []string{}

	for _, rule := range rules {
		for _, pattern := range rule.Repositories {
			if strings.Contains(pattern, "*") || seen[strings.ToLower(pattern)] {
				continue
			}

			seen[strings.ToLower(pattern)] = true
			names = append(names, pattern)
		}
	}

	sort.Strings(names)

	return names
}

// lowestCommon returns each permission every set has, at the lowest level
// any of them has.
func lowestCommon(sets []map[string]permissions.Level) map[string]permissions.Level {
	result := maps(sets[0])

	for _, set := range sets[1:] {
		for name, level := range result {
			have, ok := set[name]

			switch {
			case !ok:
				delete(result, name)
			case have.Rank() < level.Rank():
				result[name] = have
			}
		}
	}

	return result
}

func validRepositories(requested []string) ([]string, string) {
	seen := map[string]bool{}
	repositories := []string{}

	for _, repository := range requested {
		if !repositoryName.MatchString(repository) || repository == "." || repository == ".." {
			return nil, fmt.Sprintf("repository %q is not a repository name", repository)
		}

		if seen[strings.ToLower(repository)] {
			continue
		}

		seen[strings.ToLower(repository)] = true
		repositories = append(repositories, repository)
	}

	return repositories, ""
}

func validPermissions(requested map[string]permissions.Level) string {
	for _, name := range sortedKeys(requested) {
		if !permissions.ValidRepositoryPermission(name, requested[name]) {
			return fmt.Sprintf("permission %s: %s is not a known repository permission level", name, requested[name])
		}
	}

	return ""
}

// refuse builds a refusal whose message starts with the caller, so every
// refusal names its namespace and ServiceAccount.
func refuse(outcome Outcome, who, detail string) Decision {
	return Decision{Outcome: outcome, Message: who + ": " + detail}
}

func sortedKeys(set map[string]permissions.Level) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func maps(set map[string]permissions.Level) map[string]permissions.Level {
	copied := make(map[string]permissions.Level, len(set))
	for name, level := range set {
		copied[name] = level
	}

	return copied
}

func quoted(names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%q", name))
	}

	return strings.Join(parts, ", ")
}
