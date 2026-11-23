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
	// read, so a rule with pushRefs can't allow it.
	RefsUnknown bool
	// PublishesRelease is true when a release update may publish a draft
	// without naming its tag. Publishing creates the draft's tag, which the
	// proxy can't see without looking the release up, so a rule with
	// pushRefs can't allow it.
	PublishesRelease bool
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
	// Access is how an allowed request is sent: with the rules' token, with
	// the public-read token, with a user access token, or with no credential.
	Access Access
	// UserAuthorization is the authorization whose access token an allowed
	// user request is forwarded with; set only with AccessUser.
	UserAuthorization *model.UserAuthorization
	// RepositoryID is the ID a /repositories/{id}/... request named its
	// repository by, or 0. The name it was decided as may be stale, so the
	// token must reach this ID.
	RepositoryID int64
}

// Access is the credential an allowed request is forwarded with.
type Access string

// The ways an allowed request is forwarded.
const (
	// AccessRules forwards with a token of the rules that allowed it.
	AccessRules Access = "rules"
	// AccessPublicRead forwards with a token of the installation that has
	// only metadata: read and so reads public data only.
	AccessPublicRead Access = "public_read"
	// AccessWeb forwards a github.com web path with no credential.
	AccessWeb Access = "web"
	// AccessUser forwards with the current access token of a user
	// authorization.
	AccessUser Access = "user"
)

// Allowed reports whether the request may be forwarded.
func (d Decision) Allowed() bool { return d.Status == 0 }

// Decide decides a request against the caller's accepted rules. Only rules
// with proxy mode are candidates, and only the rules that allow the request
// give it a token, so one rule's lists or pushRefs can't be undone by
// another rule's permissions. Anything it can't decide is refused.
func Decide(rules []*model.Rule, request Decided) Decision {
	if decision, decided := decideUser(rules, request); decided {
		return decision
	}

	candidates := make([]*model.Rule, 0, len(rules))

	for _, rule := range rules {
		if rule.Proxy != nil && rule.Installation != nil {
			candidates = append(candidates, rule)
		}
	}

	if request.Kind == KindWeb {
		return decideWeb(candidates)
	}

	if request.Repo != "" {
		if publicReadApplies(candidates, request) {
			return decidePublicRead(candidates)
		}

		return decideRepository(candidates, request)
	}

	decision := decideWithoutRepository(candidates, request)
	if !decision.Allowed() && decision.Status == http.StatusForbidden && publicReadApplies(candidates, request) {
		return decidePublicRead(candidates)
	}

	return decision
}

// decideUser decides a request by the caller's user rules alone when one of
// them lists its operation, and reports whether it did. Only REST requests
// to api.github.com can use a user rule. An operation a user rule lists is
// never decided by the installation rules, even while that rule isn't
// accepted, so it never gets an installation token instead.
func decideUser(rules []*model.Rule, request Decided) (Decision, bool) {
	if request.Kind != KindREST || request.Host != HostAPI {
		return Decision{}, false
	}

	listing := []*model.Rule{}

	for _, rule := range rules {
		if rule.User != nil && slices.Contains(rule.User.Operations, request.Operation) {
			listing = append(listing, rule)
		}
	}

	if len(listing) == 0 {
		return Decision{}, false
	}

	slices.SortStableFunc(listing, func(left, right *model.Rule) int {
		return strings.Compare(left.Policy.String()+"/"+left.Name, right.Policy.String()+"/"+right.Name)
	})

	authorizations := map[types.NamespacedName]bool{}

	for _, rule := range listing {
		if !rule.User.Accepted {
			return denied(fmt.Sprintf("operation %q: user rule %q is not accepted: %s", request.Operation, rule.Name, rule.User.Reason)), true
		}

		authorizations[rule.User.Authorization] = true
	}

	if len(authorizations) > 1 {
		return denied(fmt.Sprintf("operation %q: covered by more than one user authorization", request.Operation)), true
	}

	authorization := listing[0].User.View
	if authorization == nil || !authorization.Resolved {
		reason := model.ReasonRefNotFound
		if authorization != nil {
			reason = authorization.Reason
		}

		return UserNotReady(listing[0].User.Authorization, reason), true
	}

	return Decision{Rules: listing, Access: AccessUser, UserAuthorization: authorization}, true
}

// UserNotReady is the refusal of a request a user authorization can't serve
// now; it never falls back to an installation token.
func UserNotReady(authorization types.NamespacedName, reason string) Decision {
	return Decision{
		Status: http.StatusServiceUnavailable, Code: codeUnavailable,
		Message: fmt.Sprintf("user authorization %s is not ready: %s", authorization, reason),
	}
}

// decideWeb allows a github.com web path when a rule turns web paths on.
func decideWeb(candidates []*model.Rule) Decision {
	allowing := []*model.Rule{}

	for _, rule := range candidates {
		if rule.Proxy.WebPaths {
			allowing = append(allowing, rule)
		}
	}

	if len(allowing) == 0 {
		return Decision{Status: http.StatusNotFound, Code: codeNotFound, Message: "only git smart HTTP is served on github.com"}
	}

	return Decision{Rules: allowing, Access: AccessWeb}
}

// publicReadApplies reports whether request is a read-only request outside
// every installation of the caller's proxy rules: a request whose {owner},
// {org} or {username} names an installation's own owner, in any case,
// always keeps the rules' decision, so public read never widens what a rule
// allows there. GraphQL and the uploads host are never public reads.
func publicReadApplies(candidates []*model.Rule, request Decided) bool {
	switch {
	case request.Host != HostAPI && request.Host != HostGit:
		return false
	case request.Kind == KindREST:
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			return false
		}
	case request.Kind == KindGitInfoRefs || request.Kind == KindGitUpload:
		if request.Contents != permissions.LevelRead {
			return false
		}
	default:
		return false
	}

	for _, named := range append([]string{request.Owner}, request.Accounts...) {
		account := unescaped(named)

		for _, rule := range candidates {
			if account != "" && strings.EqualFold(rule.Installation.Owner, account) {
				return false
			}
		}
	}

	return slices.ContainsFunc(candidates, func(rule *model.Rule) bool { return rule.Proxy.PublicRead })
}

// decidePublicRead gives a public read the public-read token: a token of
// one installation with only metadata: read, limited to the repositories
// its public-read rules name exactly. It reads public data of any owner and
// no private contents. When rules of several installations turn public read
// on, the installation that sorts first is used, as any of them reads the
// same public data.
func decidePublicRead(candidates []*model.Rule) Decision {
	allowing := []*model.Rule{}

	for _, rule := range candidates {
		if rule.Proxy.PublicRead {
			allowing = append(allowing, rule)
		}
	}

	slices.SortStableFunc(allowing, func(left, right *model.Rule) int {
		return strings.Compare(left.Installation.String(), right.Installation.String())
	})

	installation := allowing[0].Installation
	rules := []*model.Rule{}
	repositories := []string{}
	seen := map[string]bool{}

	for _, rule := range allowing {
		if rule.Installation.NamespacedName != installation.NamespacedName {
			continue
		}

		rules = append(rules, rule)

		for _, name := range exactNames(rule) {
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				repositories = append(repositories, name)
			}
		}
	}

	if len(repositories) == 0 {
		return denied("public read needs a proxy rule that names a repository without a pattern")
	}

	sort.Strings(repositories)

	if len(repositories) > maxTokenRepositories {
		repositories = repositories[:maxTokenRepositories]
	}

	decision := grantDecision(installation, repositories, map[string]permissions.Level{"metadata": permissions.LevelRead}, rules)
	if decision.Allowed() {
		decision.Access = AccessPublicRead
	}

	return decision
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
	exactReasons := []string{}

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

			if len(exactNames(rule)) > 0 {
				exactReasons = append(exactReasons, reason)
			}

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
		// The exact-name rules are the ones GraphQL could use; when they
		// refused the request, their reasons are the useful answer.
		if len(exactReasons) > 0 {
			slices.Sort(exactReasons)

			return denied(strings.Join(slices.Compact(exactReasons), "; "))
		}

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

		if request.PublishesRelease {
			return fmt.Sprintf("operation %q without tag_name can publish a draft release, which creates a tag pushRefs can't check", request.Operation)
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

	return Decision{Installation: installation, Repositories: repositories, Permissions: set, Rules: rules, Access: AccessRules}
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
