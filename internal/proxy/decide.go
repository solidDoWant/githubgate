package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/operations"
	"github.com/solidDoWant/githubgate/internal/patterns"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

// maxTokenRepositories is the most repositories GitHub accepts in one token.
const maxTokenRepositories = 500

// The error codes of decisions.
const (
	codeDenied                = "denied"
	codeAmbiguousInstallation = "ambiguous_installation"
)

// Decided is what a classified request is decided with: the classification
// and what the proxy read from the body.
type Decided struct {
	Request
	// Mutations are a GraphQL mutation's top-level field names; empty for a
	// query and for other kinds.
	Mutations []string
	// Refs are the full refs the request writes, as far as they are known.
	Refs []string
	// RefsUnknown is true when the request writes refs the proxy has not
	// read (a git push), so a rule with pushRefs can't allow it.
	RefsUnknown bool
	// push is a git push's command section, kept to answer a refusal in
	// git's protocol.
	push *receiveCommands
}

// Decision is the outcome of deciding a request against the caller's rules.
type Decision struct {
	// Status, Code and Message describe a refusal; Status is 0 when allowed.
	Status  int
	Code    string
	Message string
	// Installation, Repositories and Permissions are the token to mint, set
	// only when allowed.
	Installation *model.Installation
	Repositories []string
	Permissions  map[string]permissions.Level
	// Rules are the rules that allowed the request.
	Rules []*model.Rule
}

// Allowed reports whether the request may be forwarded.
func (d Decision) Allowed() bool { return d.Status == 0 }

// Decide decides a request against the caller's accepted rules. Only rules
// with proxy mode are candidates, and only the rules that allow the request
// give it a token, so one rule's lists or pushRefs can't be undone by
// another rule's permissions. Anything it can't decide is refused.
func Decide(rules []*model.Rule, request Decided) Decision {
	candidates := make([]*model.Rule, 0, len(rules))

	for _, rule := range rules {
		if rule.Proxy != nil && rule.Installation != nil {
			candidates = append(candidates, rule)
		}
	}

	if request.Repo != "" {
		return decideRepository(candidates, request)
	}

	return decideWithoutRepository(candidates, request)
}

func decideRepository(candidates []*model.Rule, request Decided) Decision {
	repository := unescaped(request.Repo)
	reasons := []string{}
	allowing := []*model.Rule{}

	for _, rule := range candidates {
		if !strings.EqualFold(rule.Installation.Owner, unescaped(request.Owner)) || !covers(rule, repository) {
			continue
		}

		if reason := refusedBy(rule, request); reason != "" {
			reasons = append(reasons, reason)

			continue
		}

		allowing = append(allowing, rule)
	}

	if len(allowing) == 0 {
		why := "it is not covered by any of your proxy rules"

		if len(reasons) > 0 {
			slices.Sort(reasons)
			why = strings.Join(slices.Compact(reasons), "; ")
		}

		return denied(fmt.Sprintf("repository %q: %s", unescaped(request.Owner)+"/"+repository, why))
	}

	installation, refusal := oneInstallation(allowing)
	if refusal != nil {
		return *refusal
	}

	set := map[string]permissions.Level{}

	for _, rule := range allowing {
		for name, level := range rule.Permissions {
			if level.Rank() > set[name].Rank() {
				set[name] = level
			}
		}
	}

	return grantDecision(installation, []string{repository}, set, allowing)
}

func decideWithoutRepository(candidates []*model.Rule, request Decided) Decision {
	reasons := []string{}
	allowing := []*model.Rule{}
	owner := unescaped(request.Owner)

	for _, rule := range candidates {
		if owner != "" && !strings.EqualFold(rule.Installation.Owner, owner) {
			continue
		}

		if request.Kind == KindREST && !listed(rule.Proxy.AllowOperations, request.Operation) {
			reasons = append(reasons, fmt.Sprintf("operation %q names no repository and is not in allowOperations", request.Operation))

			continue
		}

		if reason := refusedBy(rule, request); reason != "" {
			reasons = append(reasons, reason)

			continue
		}

		allowing = append(allowing, rule)
	}

	if len(allowing) == 0 {
		why := "no proxy rule allows it"

		if len(reasons) > 0 {
			slices.Sort(reasons)
			why = strings.Join(slices.Compact(reasons), "; ")
		}

		return denied(why)
	}

	exact := allowing[:0:0]
	for _, rule := range allowing {
		if len(exactNames(rule)) > 0 {
			exact = append(exact, rule)
		}
	}

	if len(exact) == 0 {
		return denied("your proxy rules only use patterns; GraphQL needs exact repository names")
	}

	installation, refusal := oneInstallation(exact)
	if refusal != nil {
		return *refusal
	}

	repositories := []string{}
	seen := map[string]bool{}

	for _, rule := range exact {
		for _, name := range exactNames(rule) {
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				repositories = append(repositories, name)
			}
		}
	}

	sort.Strings(repositories)

	if len(repositories) > maxTokenRepositories {
		return denied(fmt.Sprintf("your proxy rules name %d repositories; a token holds at most %d", len(repositories), maxTokenRepositories))
	}

	// Each repository gets the highest level any allowing rule covering it
	// gives; the token gets the lowest of those across the repositories.
	var common map[string]permissions.Level

	for index, repository := range repositories {
		given := map[string]permissions.Level{}

		for _, rule := range exact {
			if !covers(rule, repository) {
				continue
			}

			for name, level := range rule.Permissions {
				if level.Rank() > given[name].Rank() {
					given[name] = level
				}
			}
		}

		if index == 0 {
			common = given

			continue
		}

		for name, level := range common {
			have, ok := given[name]

			switch {
			case !ok:
				delete(common, name)
			case have.Rank() < level.Rank():
				common[name] = have
			}
		}
	}

	return grantDecision(installation, repositories, common, exact)
}

// refusedBy returns why rule doesn't allow request, or "" when it does. The
// repository (or its absence) is checked by the caller.
func refusedBy(rule *model.Rule, request Decided) string {
	proxy := rule.Proxy

	switch request.Kind {
	case KindREST:
		if !operationAllowed(proxy, request.Operation) {
			return fmt.Sprintf("operation %q is not allowed", request.Operation)
		}

		if proxy.PushRefs != nil {
			if reason := refsRefused(proxy, operations.RESTRefCheck(request.Operation), request); reason != "" {
				return reason
			}
		}
	case KindGraphQL:
		for _, mutation := range request.Mutations {
			if !mutationAllowed(proxy, mutation) {
				return fmt.Sprintf("mutation %q is not allowed", mutation)
			}

			if proxy.PushRefs != nil && operations.MutationRefCheck(mutation) != operations.RefsNone {
				return fmt.Sprintf("mutation %q writes refs that pushRefs can't check", mutation)
			}
		}
	case KindGitInfoRefs, KindGitUpload, KindGitReceive:
		if !rule.Permissions["contents"].AtLeast(request.Contents) {
			return fmt.Sprintf("git needs contents: %s", request.Contents)
		}

		if proxy.PushRefs != nil {
			if request.RefsUnknown {
				return "the refs pushed can't be checked against pushRefs"
			}

			if reason := refsOutside(proxy.PushRefs, request.Refs); reason != "" {
				return reason
			}
		}
	default:
		return "the request can't be classified"
	}

	return ""
}

// refusedRefs returns, for each ref of a refused push, why the push would
// be refused if it wrote that ref alone. A ref that would be allowed alone
// is left out; when every ref would be, the push is refused for what the
// refs need together, and each ref gets that reason.
func refusedRefs(rules []*model.Rule, request Decided) map[string]string {
	reasons := map[string]string{}

	for _, ref := range request.Refs {
		alone := request
		alone.Refs = []string{ref}

		if decision := Decide(rules, alone); !decision.Allowed() {
			reasons[ref] = decision.Message
		}
	}

	if len(reasons) == 0 {
		whole := Decide(rules, request)
		for _, ref := range request.Refs {
			reasons[ref] = whole.Message
		}
	}

	return reasons
}

func refsRefused(proxy *model.ProxyRule, check operations.RefCheck, request Decided) string {
	switch check {
	case operations.RefsNone:
		return ""
	case operations.RefsBodyRef, operations.RefsPathRef, operations.RefsReleaseTag:
		if request.RefsUnknown {
			return fmt.Sprintf("the refs operation %q writes can't be checked against pushRefs", request.Operation)
		}

		return refsOutside(proxy.PushRefs, request.Refs)
	default:
		return fmt.Sprintf("operation %q writes refs that pushRefs can't check", request.Operation)
	}
}

func refsOutside(pushRefs, refs []string) string {
	for _, ref := range refs {
		if !slices.ContainsFunc(pushRefs, func(pattern string) bool { return patterns.MatchSegments(pattern, ref) }) {
			return fmt.Sprintf("ref %q is outside pushRefs", ref)
		}
	}

	return ""
}

func operationAllowed(proxy *model.ProxyRule, operation string) bool {
	if proxy.AllowOperations != nil && !listed(proxy.AllowOperations, operation) {
		return false
	}

	return !listed(proxy.DenyOperations, operation)
}

func mutationAllowed(proxy *model.ProxyRule, mutation string) bool {
	if proxy.AllowMutations != nil && !listed(proxy.AllowMutations, mutation) {
		return false
	}

	return !listed(proxy.DenyMutations, mutation)
}

func listed(list []string, name string) bool {
	return slices.ContainsFunc(list, func(pattern string) bool { return patterns.MatchSegments(pattern, name) })
}

func covers(rule *model.Rule, repository string) bool {
	return slices.ContainsFunc(rule.Repositories, func(pattern string) bool { return patterns.Match(pattern, repository) })
}

func exactNames(rule *model.Rule) []string {
	names := []string{}

	for _, pattern := range rule.Repositories {
		if !strings.Contains(pattern, "*") {
			names = append(names, pattern)
		}
	}

	return names
}

// oneInstallation returns the installation of rules, or a refusal when
// there are several.
func oneInstallation(rules []*model.Rule) (*model.Installation, *Decision) {
	names := []string{}
	byName := map[types.NamespacedName]*model.Installation{}

	for _, rule := range rules {
		name := rule.Installation.NamespacedName
		if _, seen := byName[name]; !seen {
			byName[name] = rule.Installation
			names = append(names, name.String())
		}
	}

	if len(byName) == 1 {
		return rules[0].Installation, nil
	}

	sort.Strings(names)

	return nil, &Decision{
		Status: http.StatusConflict, Code: codeAmbiguousInstallation,
		Message: fmt.Sprintf("more than one installation allows this request (%s)", strings.Join(names, ", ")),
	}
}

func grantDecision(installation *model.Installation, repositories []string, set map[string]permissions.Level, rules []*model.Rule) Decision {
	if len(set) == 0 {
		return denied(fmt.Sprintf("your proxy rules give repositories %s no common permission", strings.Join(repositories, ", ")))
	}

	if !installation.Ready {
		return Decision{
			Status: http.StatusServiceUnavailable, Code: codeUnavailable,
			Message: fmt.Sprintf("installation %s is not ready: %s", installation, installation.Reason),
		}
	}

	return Decision{Installation: installation, Repositories: repositories, Permissions: set, Rules: rules}
}

func denied(message string) Decision {
	return Decision{Status: http.StatusForbidden, Code: codeDenied, Message: message}
}

// unescaped returns a path value without its percent-encoding; a value that
// can't be decoded is kept, and then matches no rule.
func unescaped(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}

	return decoded
}
