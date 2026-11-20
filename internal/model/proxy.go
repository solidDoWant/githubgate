package model

import (
	"fmt"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/operations"
	"github.com/solidDoWant/githubgate/internal/patterns"
)

// ProxyRule is the proxy mode of an accepted rule. A nil list is unset.
type ProxyRule struct {
	PushRefs        []string
	AllowOperations []string
	DenyOperations  []string
	AllowMutations  []string
	DenyMutations   []string
}

// InvalidProxyMode describes the first entry of a rule's proxy lists that is
// malformed or matches nothing in GitHub's current REST or GraphQL
// catalogue. It returns the entry's field path within the rule (for example
// "proxy.denyOperations[1]") and the problem, or two empty strings when every
// entry is valid. An entry that matches nothing is invalid so that a renamed
// operation never silently becomes allowed.
func InvalidProxyMode(proxy *githubgatev1alpha1.ProxyMode) (string, string) {
	if proxy == nil {
		return "", ""
	}

	for index, ref := range proxy.PushRefs {
		if !patterns.ValidRef(string(ref)) {
			return fmt.Sprintf("proxy.pushRefs[%d]", index), fmt.Sprintf("%q is not a valid git ref pattern", ref)
		}
	}

	lists := []struct {
		field    string
		patterns []githubgatev1alpha1.OperationPattern
	}{
		{"allowOperations", proxy.AllowOperations},
		{"denyOperations", proxy.DenyOperations},
	}
	for _, list := range lists {
		for index, pattern := range list.patterns {
			if !operations.AnyOperationMatches(string(pattern)) {
				return fmt.Sprintf("proxy.%s[%d]", list.field, index),
					fmt.Sprintf("%q matches no operation in GitHub's REST API description", pattern)
			}
		}
	}

	mutationLists := []struct {
		field    string
		patterns []githubgatev1alpha1.MutationPattern
	}{
		{"allowMutations", proxy.AllowMutations},
		{"denyMutations", proxy.DenyMutations},
	}
	for _, list := range mutationLists {
		for index, pattern := range list.patterns {
			if !operations.AnyMutationMatches(string(pattern)) {
				return fmt.Sprintf("proxy.%s[%d]", list.field, index),
					fmt.Sprintf("%q matches no mutation in GitHub's GraphQL schema", pattern)
			}
		}
	}

	return "", ""
}

// proxyRuleOf converts an enabled proxy mode to a ProxyRule, or returns nil.
func proxyRuleOf(proxy *githubgatev1alpha1.ProxyMode) *ProxyRule {
	if proxy == nil || !proxy.Enabled {
		return nil
	}

	return &ProxyRule{
		PushRefs:        stringsOf(proxy.PushRefs),
		AllowOperations: stringsOf(proxy.AllowOperations),
		DenyOperations:  stringsOf(proxy.DenyOperations),
		AllowMutations:  stringsOf(proxy.AllowMutations),
		DenyMutations:   stringsOf(proxy.DenyMutations),
	}
}

func stringsOf[T ~string](values []T) []string {
	if values == nil {
		return nil
	}

	out := make([]string, len(values))
	for index, value := range values {
		out[index] = string(value)
	}

	return out
}

func hasProxyLists(proxy *githubgatev1alpha1.ProxyMode) bool {
	return proxy != nil && (proxy.PushRefs != nil || proxy.AllowOperations != nil || proxy.DenyOperations != nil ||
		proxy.AllowMutations != nil || proxy.DenyMutations != nil)
}
