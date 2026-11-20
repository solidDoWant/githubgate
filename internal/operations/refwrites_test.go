package operations

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

// unreviewed returns the names that look as if they might write a ref but
// are missing from the table.
func unreviewed(table map[string]RefCheck, names []string) []string {
	var missing []string

	for _, name := range names {
		if _, ok := table[name]; !ok && refWriteReview.MatchString(name) {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)

	return missing
}

// stale returns the table entries that name nothing in the catalogue.
func stale(table map[string]RefCheck, names []string) []string {
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
	}

	var extra []string

	for name := range table {
		if !known[name] {
			extra = append(extra, name)
		}
	}

	sort.Strings(extra)

	return extra
}

func restIDs() []string {
	ids := make([]string, 0, len(restCatalogue))
	for _, op := range restCatalogue {
		ids = append(ids, op.ID)
	}

	return ids
}

func mutationNames() []string {
	names := make([]string, 0, len(mutationCatalogue))
	for name := range mutationCatalogue {
		names = append(names, name)
	}

	return names
}

func TestRefWritesTableIsComplete(t *testing.T) {
	assert.Empty(t, unreviewed(restRefWrites, restIDs()), "REST operations to classify in restRefWrites")
	assert.Empty(t, unreviewed(mutationRefWrites, mutationNames()), "mutations to classify in mutationRefWrites")
	assert.Empty(t, stale(restRefWrites, restIDs()), "restRefWrites entries not in the catalogue")
	assert.Empty(t, stale(mutationRefWrites, mutationNames()), "mutationRefWrites entries not in the catalogue")
}

func TestRefWritesCompletenessCatchesNewEntries(t *testing.T) {
	assert.Equal(t, []string{"git/create-refs-batch", "repos/redeploy-environment", "repos/tag-release"},
		unreviewed(restRefWrites, append(restIDs(), "repos/tag-release", "git/create-refs-batch", "repos/redeploy-environment", "issues/lock")))
	assert.Equal(t, []string{"forceMergeBranch", "redeployEnvironment"},
		unreviewed(mutationRefWrites, append(mutationNames(), "forceMergeBranch", "redeployEnvironment", "addReaction")))
}

func TestRESTRefCheck(t *testing.T) {
	tests := map[string]RefCheck{
		"git/create-ref":                       RefsBodyRef,
		"git/update-ref":                       RefsPathRef,
		"git/delete-ref":                       RefsPathRef,
		"repos/create-release":                 RefsReleaseTag,
		"repos/update-release":                 RefsReleaseTag,
		"repos/create-or-update-file-contents": RefsRefused,
		"repos/delete-file":                    RefsRefused,
		"repos/merge":                          RefsRefused,
		"repos/rename-branch":                  RefsRefused,
		"repos/merge-upstream":                 RefsRefused,
		"pulls/merge":                          RefsRefused,
		"pulls/update-branch":                  RefsRefused,
		"repos/create-deployment":              RefsRefused,
		"repos/delete-release":                 RefsNone,
		"git/create-commit":                    RefsNone,
		"issues/create":                        RefsNone,
		"pulls/create":                         RefsNone,
		// Not yet reviewed: refused until classified.
		"repos/force-push-branch": RefsRefused,
	}

	for id, want := range tests {
		assert.Equal(t, want, RESTRefCheck(id), id)
	}
}

func TestMutationRefCheck(t *testing.T) {
	for _, name := range []string{
		"createRef", "updateRef", "updateRefs", "deleteRef", "createCommitOnBranch", "mergeBranch",
		"mergePullRequest", "enablePullRequestAutoMerge", "updatePullRequestBranch", "revertPullRequest",
		"createDeployment", "forceMergeBranch",
	} {
		assert.Equal(t, RefsRefused, MutationRefCheck(name), name)
	}

	for _, name := range []string{"createPullRequest", "addComment", "createBranchProtectionRule"} {
		assert.Equal(t, RefsNone, MutationRefCheck(name), name)
	}
}
