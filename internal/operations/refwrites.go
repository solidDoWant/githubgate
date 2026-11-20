package operations

import "regexp"

// RefCheck says which git refs an operation writes and how a rule's pushRefs
// applies to it.
type RefCheck int

const (
	// RefsNone: the operation writes no ref.
	RefsNone RefCheck = iota
	// RefsBodyRef: the body's "ref" field is the full ref written.
	RefsBodyRef
	// RefsPathRef: "refs/" followed by the {ref} path parameter is written.
	RefsPathRef
	// RefsReleaseTag: "refs/tags/" followed by the body's "tag_name" is
	// written (GitHub creates the tag if it is missing); a body without
	// "tag_name" writes nothing.
	RefsReleaseTag
	// RefsRefused: the operation writes a ref githubgate can't check without
	// asking GitHub (a default branch, a ref by node id), so it is refused
	// whenever pushRefs is set.
	RefsRefused
)

// refWriteReview matches the REST operation ids and mutation names that must
// appear in the tables below. A catalogue update adding one fails the
// completeness test until someone reviews it.
var refWriteReview = regexp.MustCompile(`(?i)ref|commit|merge|branch|tag|release|content|file|deploy`)

// restRefWrites is the reviewed table of REST operations. Reads never write.
var restRefWrites = map[string]RefCheck{
	"git/create-ref": RefsBodyRef,
	"git/update-ref": RefsPathRef,
	"git/delete-ref": RefsPathRef,

	"repos/create-release": RefsReleaseTag,
	"repos/update-release": RefsReleaseTag,

	// The branch defaults to the default branch, unknown without a lookup.
	"repos/create-or-update-file-contents": RefsRefused,
	"repos/delete-file":                    RefsRefused,

	// These write a branch named by a pull request, a merge or a rename.
	"repos/merge":                            RefsRefused,
	"repos/rename-branch":                    RefsRefused,
	"repos/merge-upstream":                   RefsRefused,
	"pulls/merge":                            RefsRefused,
	"pulls/merge-async":                      RefsRefused,
	"pulls/update-branch":                    RefsRefused,
	"code-scanning/commit-autofix":           RefsRefused,
	"agent-merge/put-agent-merge-enrollment": RefsRefused,
	// A source import pushes the imported history; updating its settings
	// can restart it.
	"migrations/map-commit-author":  RefsRefused,
	"migrations/set-lfs-preference": RefsRefused,
	// A deployment can auto-merge the default branch into its ref, and
	// auto-merge is on unless the body turns it off.
	"repos/create-deployment": RefsRefused,

	// Reviewed: these write no ref.
	"agent-merge/delete-agent-merge-enrollment":              RefsNone,
	"agent-merge/get-agent-merge-enrollment":                 RefsNone,
	"checks/list-for-ref":                                    RefsNone,
	"checks/list-suites-for-ref":                             RefsNone,
	"checks/set-suites-preferences":                          RefsNone,
	"copilot/copilot-content-exclusion-for-organization":     RefsNone,
	"copilot/set-copilot-content-exclusion-for-organization": RefsNone,
	"git/create-commit":                                      RefsNone,
	"git/create-tag":                                         RefsNone,
	"git/get-commit":                                         RefsNone,
	"git/get-ref":                                            RefsNone,
	"git/get-tag":                                            RefsNone,
	"git/list-matching-refs":                                 RefsNone,
	"migrations/get-commit-authors":                          RefsNone,
	"migrations/get-large-files":                             RefsNone,
	"orgs/disable-selected-repository-immutable-releases-organization": RefsNone,
	"orgs/enable-selected-repository-immutable-releases-organization":  RefsNone,
	"orgs/get-immutable-releases-settings":                             RefsNone,
	"orgs/get-immutable-releases-settings-repositories":                RefsNone,
	"orgs/set-immutable-releases-settings":                             RefsNone,
	"orgs/set-immutable-releases-settings-repositories":                RefsNone,
	"reactions/create-for-commit-comment":                              RefsNone,
	"reactions/create-for-release":                                     RefsNone,
	"reactions/delete-for-commit-comment":                              RefsNone,
	"reactions/delete-for-release":                                     RefsNone,
	"reactions/list-for-commit-comment":                                RefsNone,
	"reactions/list-for-release":                                       RefsNone,
	"repos/check-immutable-releases":                                   RefsNone,
	"repos/compare-commits":                                            RefsNone,
	"repos/create-commit-comment":                                      RefsNone,
	"repos/create-commit-signature-protection":                         RefsNone,
	"repos/create-commit-status":                                       RefsNone,
	"repos/create-deployment-branch-policy":                            RefsNone,
	"repos/delete-admin-branch-protection":                             RefsNone,
	"repos/delete-branch-protection":                                   RefsNone,
	"repos/delete-commit-comment":                                      RefsNone,
	"repos/delete-commit-signature-protection":                         RefsNone,
	"repos/delete-deployment-branch-policy":                            RefsNone,
	"repos/delete-release":                                             RefsNone,
	"repos/delete-release-asset":                                       RefsNone,
	"repos/disable-immutable-releases":                                 RefsNone,
	"repos/enable-immutable-releases":                                  RefsNone,
	"repos/generate-release-notes":                                     RefsNone,
	"repos/set-admin-branch-protection":                                RefsNone,
	"repos/update-branch-protection":                                   RefsNone,
	"repos/update-commit-comment":                                      RefsNone,
	"repos/update-deployment-branch-policy":                            RefsNone,
	"repos/update-release-asset":                                       RefsNone,
	"repos/upload-release-asset":                                       RefsNone,
	"gists/list-commits":                                               RefsNone,
	"pulls/check-if-merged":                                            RefsNone,
	"pulls/get-merge-async-result":                                     RefsNone,
	"pulls/list-commits":                                               RefsNone,
	"pulls/list-files":                                                 RefsNone,
	"repos/get-admin-branch-protection":                                RefsNone,
	"repos/get-apps-with-access-to-protected-branch":                   RefsNone,
	"repos/get-branch":                                                 RefsNone,
	"repos/get-branch-protection":                                      RefsNone,
	"repos/get-branch-rules":                                           RefsNone,
	"repos/get-combined-status-for-ref":                                RefsNone,
	"repos/get-commit":                                                 RefsNone,
	"repos/get-commit-activity-stats":                                  RefsNone,
	"repos/get-commit-comment":                                         RefsNone,
	"repos/get-commit-signature-protection":                            RefsNone,
	"repos/get-community-profile-metrics":                              RefsNone,
	"repos/get-content":                                                RefsNone,
	"repos/get-deployment-branch-policy":                               RefsNone,
	"repos/get-latest-release":                                         RefsNone,
	"repos/get-release":                                                RefsNone,
	"repos/get-release-asset":                                          RefsNone,
	"repos/get-release-by-tag":                                         RefsNone,
	"repos/get-teams-with-access-to-protected-branch":                  RefsNone,
	"repos/get-top-referrers":                                          RefsNone,
	"repos/get-users-with-access-to-protected-branch":                  RefsNone,
	"repos/list-branches":                                              RefsNone,
	"repos/list-branches-for-head-commit":                              RefsNone,
	"repos/list-comments-for-commit":                                   RefsNone,
	"repos/list-commit-comments-for-repo":                              RefsNone,
	"repos/list-commit-statuses-for-ref":                               RefsNone,
	"repos/list-commits":                                               RefsNone,
	"repos/list-deployment-branch-policies":                            RefsNone,
	"repos/list-pull-requests-associated-with-commit":                  RefsNone,
	"repos/list-release-assets":                                        RefsNone,
	"repos/list-releases":                                              RefsNone,
	"repos/list-tags":                                                  RefsNone,
	"search/commits":                                                   RefsNone,
	"actions/get-pending-deployments-for-run":                          RefsNone,
	"actions/review-pending-deployments-for-run":                       RefsNone,
	"orgs/create-artifact-deployment-record":                           RefsNone,
	"orgs/create-cluster-deployment-records-job":                       RefsNone,
	"orgs/get-cluster-deployment-records-job":                          RefsNone,
	"orgs/list-artifact-deployment-records":                            RefsNone,
	"orgs/set-cluster-deployment-records":                              RefsNone,
	"repos/cancel-pages-deployment":                                    RefsNone,
	"repos/create-deploy-key":                                          RefsNone,
	"repos/create-deployment-protection-rule":                          RefsNone,
	"repos/create-deployment-status":                                   RefsNone,
	"repos/create-pages-deployment":                                    RefsNone,
	"repos/delete-deploy-key":                                          RefsNone,
	"repos/delete-deployment":                                          RefsNone,
	"repos/disable-deployment-protection-rule":                         RefsNone,
	"repos/get-all-deployment-protection-rules":                        RefsNone,
	"repos/get-custom-deployment-protection-rule":                      RefsNone,
	"repos/get-deploy-key":                                             RefsNone,
	"repos/get-deployment":                                             RefsNone,
	"repos/get-deployment-status":                                      RefsNone,
	"repos/get-pages-deployment":                                       RefsNone,
	"repos/list-custom-deployment-rule-integrations":                   RefsNone,
	"repos/list-deploy-keys":                                           RefsNone,
	"repos/list-deployment-statuses":                                   RefsNone,
	"repos/list-deployments":                                           RefsNone,
}

// mutationRefWrites is the reviewed table of GraphQL mutations. A mutation
// names refs by node id, which githubgate can't check, so every mutation
// that writes a ref is refused under pushRefs.
var mutationRefWrites = map[string]RefCheck{
	"createCommitOnBranch":       RefsRefused,
	"createRef":                  RefsRefused,
	"deleteRef":                  RefsRefused,
	"enablePullRequestAutoMerge": RefsRefused,
	"mergeBranch":                RefsRefused,
	"mergePullRequest":           RefsRefused,
	"revertPullRequest":          RefsRefused,
	"updatePullRequestBranch":    RefsRefused,
	"updateRef":                  RefsRefused,
	"updateRefs":                 RefsRefused,
	// Links a new or existing branch to an issue; it can create the branch.
	"createLinkedBranch": RefsRefused,
	// Merging from a queue writes the base branch.
	"enqueuePullRequest": RefsRefused,
	// A deployment can auto-merge the default branch into its ref.
	"createDeployment": RefsRefused,

	// Reviewed: these write no ref.
	"createBranchProtectionRule":                               RefsNone,
	"deleteBranchProtectionRule":                               RefsNone,
	"deleteLinkedBranch":                                       RefsNone,
	"disablePullRequestAutoMerge":                              RefsNone,
	"markFileAsViewed":                                         RefsNone,
	"unmarkFileAsViewed":                                       RefsNone,
	"updateBranchProtectionRule":                               RefsNone,
	"updateCheckSuitePreferences":                              RefsNone,
	"updateEnterpriseMembersCanUpdateProtectedBranchesSetting": RefsNone,
	"updateEnterpriseProfile":                                  RefsNone,
	"updateOrganizationWebCommitSignoffSetting":                RefsNone,
	"updateRepositoryWebCommitSignoffSetting":                  RefsNone,
	"updateSponsorshipPreferences":                             RefsNone,
	"approveDeployments":                                       RefsNone,
	"createDeploymentStatus":                                   RefsNone,
	"deleteDeployment":                                         RefsNone,
	"rejectDeployments":                                        RefsNone,
	"updateEnterpriseDeployKeySetting":                         RefsNone,
}

// RESTRefCheck returns how pushRefs applies to a REST operation. An
// operation whose id looks as if it might write a ref but that nobody has
// reviewed yet is refused (fail closed).
func RESTRefCheck(id string) RefCheck {
	return refCheck(restRefWrites, id)
}

// MutationRefCheck returns how pushRefs applies to a GraphQL mutation, with
// the same fail-closed rule as RESTRefCheck.
func MutationRefCheck(name string) RefCheck {
	return refCheck(mutationRefWrites, name)
}

func refCheck(table map[string]RefCheck, name string) RefCheck {
	if check, ok := table[name]; ok {
		return check
	}

	if refWriteReview.MatchString(name) {
		return RefsRefused
	}

	return RefsNone
}
