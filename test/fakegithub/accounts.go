package fakegithub

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// GitHub's pagination Link headers name an account by its numeric ID, not
// its login: page 2 of GET /orgs/{org}/repos is
// /organizations/{org_id}/repos?page=2, and page 2 of
// GET /users/{username}/repos is /user/{account_id}/repos?page=2. GitHub's
// REST description has no operations for these paths. Source: GitHub's
// pagination guide, which says to follow the Link header's URLs as given
// rather than build them
// (https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api),
// and the links go-github, octokit and gh api --paginate follow. Documented,
// not yet observed against github.com by the owner's live runs.
//
// The fake serves the organization form only: a /user/{id}/... pattern
// would overlap the /user/ssh_signing_keys/{id} routes in the mux.

// accountIDLocked returns owner's account ID, giving it one at first use.
// s.mu must be held.
func (s *Server) accountIDLocked(owner string) int64 {
	key := strings.ToLower(owner)

	if s.accounts == nil {
		s.accounts = map[string]int64{}
	}

	id, ok := s.accounts[key]
	if !ok {
		s.nextID++
		id = s.nextID
		s.accounts[key] = id
	}

	return id
}

// AccountID returns the account ID of owner, as GitHub's installation
// listing and pagination links name it.
func (s *Server) AccountID(owner string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.accountIDLocked(owner)
}

// handleListOrganizationRepositories lists the repositories of an
// organization the token can see, a page at a time: the token's own
// repositories and every public one. Its next-page link names the
// organization by ID.
func (s *Server) handleListOrganizationRepositories(writer http.ResponseWriter, request *http.Request, token tokenContext) {
	org := request.PathValue("org")

	perPage, page := 30, 1
	if value, err := strconv.Atoi(request.URL.Query().Get("per_page")); err == nil && value > 0 {
		perPage = min(value, 100)
	}

	if value, err := strconv.Atoi(request.URL.Query().Get("page")); err == nil && value > 0 {
		page = value
	}

	s.mu.Lock()

	visible := []*Repository{}

	for _, repo := range s.repos {
		if strings.EqualFold(repo.Owner, org) && (!repo.Private || token.hasRepo(repo.Owner, repo.Name)) {
			visible = append(visible, repo)
		}
	}

	accountID := s.accountIDLocked(org)
	s.mu.Unlock()

	slices.SortFunc(visible, func(a, b *Repository) int { return strings.Compare(a.Name, b.Name) })

	start := min((page-1)*perPage, len(visible))
	end := min(start+perPage, len(visible))

	if end < len(visible) {
		query := request.URL.Query()
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(perPage))
		next := url.URL{Scheme: "http", Host: request.Host, Path: fmt.Sprintf("/organizations/%d/repos", accountID), RawQuery: query.Encode()}
		writer.Header().Set("Link", `<`+next.String()+`>; rel="next"`)
	}

	out := []map[string]any{}

	for _, repo := range visible[start:end] {
		out = append(out, map[string]any{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name, "private": repo.Private})
	}

	writeJSON(writer, http.StatusOK, out)
}

// byOrganizationID serves a /organizations/{id}/... path as the
// /orgs/{org}/... path of the account with that ID.
func (s *Server) byOrganizationID(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)

		s.mu.Lock()

		login := ""

		for owner, accountID := range s.accounts {
			if accountID == id {
				login = owner
			}
		}
		s.mu.Unlock()

		if err != nil || login == "" {
			writeJSONError(writer, http.StatusNotFound, "Not Found")

			return
		}

		request.SetPathValue("org", login)
		next(writer, request)
	}
}
