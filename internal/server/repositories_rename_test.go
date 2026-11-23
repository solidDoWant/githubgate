package server_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repository renamed while its ID's name is cached, whose old name is
// then taken by a new repository: a request by the old repository's ID
// never reaches the new repository, whether the caller's token was minted
// before the rename or after it.
func TestProxyRepositoryIDAfterRenameAndNameReuse(t *testing.T) {
	f, handler := repositoryIDFixture(t)
	f.inputs.Policies[0].Spec.ServiceAccounts = []string{"ci-build", "ci-deploy"}
	f.rebuild()

	for index := range 3 {
		f.fake.AddIssueComment(owner, "foo", fmt.Sprintf("old foo comment %d", index))
	}

	oldID, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)

	// ci-build pages through foo: the ID's name is now cached, and
	// ci-build's token was minted while foo had that ID.
	items, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", "/repos/"+owner+"/foo/issues/comments?per_page=2")
	require.Nil(t, refused)
	require.Len(t, items, 3)

	require.NoError(t, f.fake.RenameRepository(owner, "foo", "foo-old"))
	require.NoError(t, f.fake.CreateRepository(owner, "foo", false))
	f.fake.AddIssueComment(owner, "foo", "NEW foo comment")

	newID, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)
	require.NotEqual(t, oldID, newID)

	byOldID := fmt.Sprintf("/repositories/%d/issues/comments", oldID)

	t.Run("a token minted before the rename does not reach the new repository", func(t *testing.T) {
		items, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", byOldID)
		require.NotNil(t, refused, "a request by the renamed repository's ID was answered with another repository's data: %v", items)
		assert.NotContains(t, refused.Body.String(), "NEW foo comment")
	})

	t.Run("a token minted after the rename is refused by the repository's ID", func(t *testing.T) {
		items, _, refused := paginate(t, handler, tenantNamespace+"/ci-deploy", byOldID)
		require.NotNil(t, refused, "a request by the renamed repository's ID was answered with another repository's data: %v", items)
		assert.Equal(t, http.StatusForbidden, refused.Code)
		assert.Contains(t, refused.Body.String(), `"denied"`)
		assert.Contains(t, refused.Body.String(), fmt.Sprintf("repository ID %d", oldID))
		assert.NotContains(t, refused.Body.String(), "foo")
	})

	t.Run("the next request decides the ID as the renamed repository", func(t *testing.T) {
		before := len(f.fake.MintLog())

		_, _, refused := paginate(t, handler, tenantNamespace+"/ci-deploy", byOldID)
		require.NotNil(t, refused)
		assert.Equal(t, http.StatusForbidden, refused.Code)
		assert.Contains(t, refused.Body.String(), `"denied"`)
		assert.NotContains(t, refused.Body.String(), "foo")
		// The ID was named again, as foo-old, which no rule covers: no
		// token is minted for it.
		assert.Len(t, f.fake.MintLog(), before+1)
	})
}
