package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/solidDoWant/githubgate/internal/permissions"
)

// ErrRepositoryNotReached is a repository ID the installation doesn't reach:
// it doesn't exist, or isn't selected for the installation.
var ErrRepositoryNotReached = errors.New("the installation doesn't reach the repository")

// repositoryLookupBody asks for a token of one repository, by ID, with
// metadata: read only.
type repositoryLookupBody struct {
	RepositoryIDs []int64                      `json:"repository_ids"`
	Permissions   map[string]permissions.Level `json:"permissions"`
}

type repositoryLookupResponse struct {
	Repositories []struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repositories"`
}

// RepositoryName returns the owner and name of the repository with id, as
// the installation on owner sees it. GitHub has no lookup by ID for an App,
// so it asks for a metadata: read token of that one repository and reads
// the name from the answer; the token itself is dropped unused. It returns
// ErrRepositoryNotReached when the installation doesn't reach the
// repository, which GitHub answers with 422.
func (c *AppClient) RepositoryName(ctx context.Context, owner string, id int64) (string, string, error) {
	installationID, err := c.InstallationID(ctx, owner)
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	body := repositoryLookupBody{RepositoryIDs: []int64{id}, Permissions: map[string]permissions.Level{"metadata": permissions.LevelRead}}

	request, err := c.api.NewRequest(ctx, http.MethodPost, fmt.Sprintf("app/installations/%d/access_tokens", installationID), body)
	if err != nil {
		return "", "", unavailable("building the repository lookup failed")
	}

	var response repositoryLookupResponse
	if httpResponse, err := c.api.Do(request, &response); err != nil {
		if httpResponse != nil && httpResponse.StatusCode == http.StatusUnprocessableEntity {
			return "", "", ErrRepositoryNotReached
		}

		return "", "", classify(err, c.now())
	}

	if len(response.Repositories) != 1 || response.Repositories[0].ID != id || response.Repositories[0].Name == "" ||
		!strings.EqualFold(response.Repositories[0].Owner.Login, owner) {
		return "", "", unavailable("GitHub answered the repository lookup with another repository")
	}

	repository := response.Repositories[0]

	return repository.Owner.Login, repository.Name, nil
}
