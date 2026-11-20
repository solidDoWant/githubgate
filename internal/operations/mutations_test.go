package operations

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsMutation(t *testing.T) {
	for _, name := range []string{
		"createPullRequest", "mergePullRequest", "createRef", "updateRefs", "createCommitOnBranch", "addComment",
	} {
		assert.True(t, IsMutation(name), name)
	}

	for _, name := range []string{"", "repository", "viewer", "CreatePullRequest", "createpullrequest", "noSuchMutation"} {
		assert.False(t, IsMutation(name), name)
	}
}
