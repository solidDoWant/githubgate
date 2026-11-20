package model_test

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

// The CRD's limits on a GitHubAccessPolicy, written out here rather than read
// from the API types so a change to them shows up as a failing test.
const (
	maxServiceAccounts = 256
	maxRules           = 256
	maxRepositories    = 256
	maxNameLength      = 253
	maxRepositoryLen   = 100
)

// memoryBudget is the most one Build of a single maximal policy may allocate.
// It is a quarter of the chart's default memory limit of 128 MiB, leaving room
// for the previous snapshot, the informer caches and serving.
const memoryBudget = 32 << 20

// largePolicyInputs returns inputs with one policy in the tenant namespace
// whose every rule is accepted.
func largePolicyInputs(serviceAccounts, rules, repositories int, padding int) model.Inputs {
	in := baseInputs()
	in.Installations = []githubgatev1alpha1.GitHubInstallation{
		installation(adminNamespace, "main", permissionSet("write", "*")),
	}

	names := make([]string, 0, serviceAccounts)
	for index := range serviceAccounts {
		names = append(names, padded("s"+strconv.Itoa(index), padding))
	}

	accessRules := make([]githubgatev1alpha1.AccessRule, 0, rules)
	for index := range rules {
		patterns := make([]githubgatev1alpha1.Repository, 0, repositories)
		for repository := range repositories {
			patterns = append(patterns, githubgatev1alpha1.Repository(padded("p"+strconv.Itoa(repository), min(padding, maxRepositoryLen))))
		}

		accessRules = append(accessRules, rule("r"+strconv.Itoa(index), "main", contents("read"), patterns...))
	}

	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "policy"},
		Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
			ServiceAccounts: names,
			Rules:           accessRules,
		},
	}}

	return in
}

// padded makes name exactly length characters long, keeping it unique and a
// valid ServiceAccount and repository name.
func padded(name string, length int) string {
	if len(name) >= length {
		return name
	}

	return name + strings.Repeat("a", length-len(name))
}

// allocatedByBuild returns the bytes one Build of the inputs allocates, and
// checks that every rule was accepted for the first ServiceAccount.
func allocatedByBuild(t *testing.T, in model.Inputs) uint64 {
	t.Helper()

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	snapshot := model.Build(in)

	runtime.ReadMemStats(&after)

	policy := in.Policies[0]
	assert.Len(t, snapshot.Rules(tenantNamespace, policy.Spec.ServiceAccounts[0]), len(policy.Spec.Rules))
	assert.Len(t, snapshot.Rules(tenantNamespace, policy.Spec.ServiceAccounts[len(policy.Spec.ServiceAccounts)-1]), len(policy.Spec.Rules))

	return after.TotalAlloc - before.TotalAlloc
}

func TestBuildMemoryGrowsLinearlyWithPolicySize(t *testing.T) {
	// Thousands of ServiceAccounts and rules, beyond the CRD's limits: before
	// the snapshot shared each policy's rules, this one Build allocated
	// hundreds of MiB.
	const count = 3000

	full := allocatedByBuild(t, largePolicyInputs(count, count, 1, 0))
	onlyServiceAccounts := allocatedByBuild(t, largePolicyInputs(count, 1, 1, 0))
	onlyRules := allocatedByBuild(t, largePolicyInputs(1, count, 1, 0))

	assert.LessOrEqual(t, full, 2*(onlyServiceAccounts+onlyRules),
		"a Build must grow with ServiceAccounts plus rules, not their product")
	assert.Less(t, full, uint64(memoryBudget))
}

func TestBuildOfMaximalPolicyStaysWithinMemoryBudget(t *testing.T) {
	in := largePolicyInputs(maxServiceAccounts, maxRules, maxRepositories, maxNameLength)
	require.Len(t, in.Policies[0].Spec.ServiceAccounts[0], maxNameLength)
	require.Len(t, string(in.Policies[0].Spec.Rules[0].Repositories[0]), maxRepositoryLen)

	assert.Less(t, allocatedByBuild(t, in), uint64(memoryBudget))
}

func TestRulesJoinEveryPolicyNamingTheServiceAccount(t *testing.T) {
	in := baseInputs()

	first := policy(tenantNamespace, rule("a", "main", contents("read"), "foo"), rule("b", "main", contents("write"), "bar-1"))
	first.Name = "first"
	first.Spec.ServiceAccounts = []string{"ci-build", "ci-build"}

	rejected := policy(tenantNamespace, rule("outside", "main", contents("read"), "other"))
	rejected.Name = "rejected"

	second := policy(tenantNamespace, rule("c", "main", contents("read"), "bar-2"))
	second.Name = "second"
	second.Spec.ServiceAccounts = []string{"ci-release", "ci-build"}

	in.Policies = []githubgatev1alpha1.GitHubAccessPolicy{first, rejected, second}

	snapshot := model.Build(in)

	ruleNames := func(serviceAccount string) []string {
		names := []string{}
		for _, accepted := range snapshot.Rules(tenantNamespace, serviceAccount) {
			names = append(names, accepted.Policy.Name+"/"+accepted.Name)
		}

		return names
	}

	assert.Equal(t, []string{"first/a", "first/b", "second/c"}, ruleNames("ci-build"))
	assert.Equal(t, []string{"second/c"}, ruleNames("ci-release"))
	assert.Empty(t, snapshot.Rules(tenantNamespace, "ci-other"))
}
