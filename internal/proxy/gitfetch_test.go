package proxy_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitAndBranch adds an empty commit to work and points a new branch at it.
func commitAndBranch(t *testing.T, work, branch string) {
	t.Helper()

	runGit(t, work, "-c", "user.name=a", "-c", "user.email=a@example.com", "commit", "-q", "--allow-empty", "-m", branch)
	runGit(t, work, "branch", branch)
}

// TestGitCloneAndFetchThroughProxy covers git compressing an upload-pack
// body with gzip once it is over 1 KiB: many wanted refs on a clone, many
// local commits to negotiate on a fetch.
func TestGitCloneAndFetchThroughProxy(t *testing.T) {
	tests := []struct {
		name           string
		remoteBranches int
		localCommits   int
	}{
		{name: "a clone of a few branches", remoteBranches: 5},
		{name: "a clone of many branches", remoteBranches: 60},
		{name: "a fetch with many local commits", remoteBranches: 1, localCommits: 60},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote, bare := gitRemoteThroughProxy(t, agentRule())

			seed := t.TempDir()
			runGit(t, seed, "init", "-q", "-b", "main")
			runGit(t, seed, "-c", "user.name=a", "-c", "user.email=a@example.com", "commit", "-q", "--allow-empty", "-m", "base")

			for index := range test.remoteBranches {
				commitAndBranch(t, seed, "b"+strconv.Itoa(index))
			}

			runGit(t, seed, "push", "-q", bare, "refs/heads/*:refs/heads/*")

			clone := filepath.Join(t.TempDir(), "clone")
			proxied := []string{"-c", "http.sslVerify=false", "-c", "http.extraHeader=Authorization: Bearer " + validToken}

			output, err := gitCommand(t, t.TempDir(), append(proxied, "clone", "-q", remote, clone)...).CombinedOutput()
			require.NoError(t, err, "%s", output)

			branches := runGit(t, clone, "branch", "-r", "--format=%(refname)")
			assert.Len(t, strings.Fields(branches), test.remoteBranches+1, "every branch and main")

			if test.localCommits == 0 {
				return
			}

			for index := range test.localCommits {
				commitAndBranch(t, clone, "local"+strconv.Itoa(index))
			}

			commitAndBranch(t, seed, "later")
			runGit(t, seed, "push", "-q", bare, "refs/heads/*:refs/heads/*")

			output, err = gitCommand(t, clone, append(proxied, "fetch", "-q", "origin")...).CombinedOutput()
			require.NoError(t, err, "%s", output)

			assert.Equal(t, strings.TrimSpace(runGit(t, seed, "rev-parse", "later")),
				strings.TrimSpace(runGit(t, clone, "rev-parse", "origin/later")))
		})
	}
}
