package server_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repository renamed while its ID's name is cached, whose old name is
// then reused by a new repository: a request by the old repository's ID
// must not reach the new repository.
func TestProxyRepositoryIDAfterRenameAndNameReuse(t *testing.T) {
	f, handler := repositoryIDFixture(t)

	for index := range 3 {
		f.fake.AddIssueComment(owner, "foo", fmt.Sprintf("old foo comment %d", index))
	}

	oldID, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)

	// A caller pages through foo: the ID's name is now cached.
	items, _, refused := paginate(t, handler, tenantNamespace+"/ci-build", "/repos/"+owner+"/foo/issues/comments?per_page=2")
	require.Nil(t, refused)
	require.Len(t, items, 3)

	// foo is renamed to foo-old and a new foo is created.
	require.NoError(t, f.fake.RenameRepository(owner, "foo", "foo-old"))
	require.NoError(t, f.fake.CreateRepository(owner, "foo", false))
	f.fake.AddIssueComment(owner, "foo", "NEW foo comment")

	newID, ok := f.fake.RepositoryID(owner, "foo")
	require.True(t, ok)
	require.NotEqual(t, oldID, newID)

	// The ID now names foo-old, which no rule covers.
	items, _, refused = paginate(t, handler, tenantNamespace+"/ci-build", fmt.Sprintf("/repositories/%d/issues/comments", oldID))
	if refused != nil {
		t.Logf("refused: %d %s", refused.Code, refused.Body.String())
	}

	for _, item := range items {
		t.Logf("got comment %v", item["body"])
	}

	require.NotNil(t, refused, "a request by the renamed repository's ID was answered with another repository's data: %v", items)
	assert.Equal(t, http.StatusForbidden, refused.Code)
}
