package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)

	_ = json.NewEncoder(writer).Encode(body)
}

func (s *Server) handleApp(writer http.ResponseWriter, request *http.Request) {
	app, ok := s.authenticateApp(writer, request)
	if !ok {
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{"id": app.ID, "name": app.Name, "slug": app.Name})
}

// handleListInstallations paginates with per_page and page, like GitHub, which
// caps per_page at 100 and defaults it to 30:
// https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api
func (s *Server) handleListInstallations(writer http.ResponseWriter, request *http.Request) {
	app, ok := s.authenticateApp(writer, request)
	if !ok {
		return
	}

	perPage, page := 30, 1
	if value, err := strconv.Atoi(request.URL.Query().Get("per_page")); err == nil && value > 0 {
		perPage = min(value, 100)
	}

	if value, err := strconv.Atoi(request.URL.Query().Get("page")); err == nil && value > 0 {
		page = value
	}

	s.mu.Lock()

	var all []*Installation

	for _, inst := range s.installations {
		if inst.AppID == app.ID {
			all = append(all, inst)
		}
	}
	s.mu.Unlock()

	slices.SortFunc(all, func(a, b *Installation) int { return int(a.ID - b.ID) })

	start := min((page-1)*perPage, len(all))
	end := min(start+perPage, len(all))

	if end < len(all) {
		next := *request.URL
		query := next.Query()
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = query.Encode()
		writer.Header().Set("Link", `<http://`+request.Host+next.RequestURI()+`>; rel="next"`)
	}

	out := []map[string]any{}

	for _, inst := range all[start:end] {
		out = append(out, map[string]any{
			"id":                   inst.ID,
			"app_id":               inst.AppID,
			"account":              map[string]any{"login": inst.Owner},
			"permissions":          withMetadata(inst.Permissions),
			"repository_selection": selection(inst),
			"suspended_at":         suspendedAt(inst),
		})
	}

	writeJSON(writer, http.StatusOK, out)
}

func suspendedAt(inst *Installation) any {
	if inst.Suspended {
		return time.Now().UTC().Format(time.RFC3339)
	}

	return nil
}

func selection(inst *Installation) string {
	if inst.Repositories == nil {
		return "all"
	}

	return "selected"
}

// withMetadata adds metadata: read, which every installation token has:
// https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
func withMetadata(perms map[string]string) map[string]string {
	out := map[string]string{"metadata": "read"}

	for name, level := range perms {
		if name == "metadata" && levelRank[level] < levelRank["read"] {
			continue
		}

		out[name] = level
	}

	return out
}

type accessTokenRequest struct {
	Repositories  []string          `json:"repositories"`
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type accessTokenResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []map[string]any  `json:"repositories,omitempty"`
}

func (s *Server) handleAccessTokens(writer http.ResponseWriter, request *http.Request) {
	app, ok := s.authenticateApp(writer, request)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)

	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.installations[id]
	if err != nil || inst == nil || inst.AppID != app.ID {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	if inst.Suspended {
		writeJSONError(writer, http.StatusForbidden, "This installation has been suspended")

		return
	}

	var body accessTokenRequest
	if request.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
			writeJSONError(writer, http.StatusBadRequest, "Problems parsing JSON")

			return
		}
	}

	// At most 500 repositories by name:
	// https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
	if len(body.Repositories) > 500 {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed: repositories must have at most 500 items")

		return
	}

	granted := withMetadata(inst.Permissions)
	perms := granted

	if body.Permissions != nil {
		perms = map[string]string{"metadata": "read"}

		for name, level := range body.Permissions {
			if levelRank[level] == 0 || levelRank[level] > levelRank[granted[name]] {
				writeJSONError(writer, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")

				return
			}

			perms[name] = level
		}
	}

	if body.RepositoryIDs != nil {
		// Repositories can also be named by ID; an ID the installation
		// doesn't reach is refused like such a name.
		for _, id := range body.RepositoryIDs {
			repo := s.repoByIDLocked(id)
			if repo == nil || repo.Owner != inst.Owner {
				writeJSONError(writer, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")

				return
			}

			body.Repositories = append(body.Repositories, repo.Name)
		}
	}

	var repoNames []string

	if body.Repositories != nil {
		for _, name := range body.Repositories {
			if !s.installationReaches(inst, name) {
				writeJSONError(writer, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")

				return
			}
		}

		repoNames = body.Repositories
	} else {
		for _, repo := range s.repos {
			if repo.Owner == inst.Owner && s.installationReaches(inst, repo.Name) {
				repoNames = append(repoNames, repo.Name)
			}
		}
	}

	slices.Sort(repoNames)

	now := s.nowLocked()
	issued := IssuedToken{
		Token:          newToken(),
		InstallationID: inst.ID,
		Repositories:   append([]string{}, repoNames...),
		Permissions:    perms,
		ExpiresAt:      now.Add(s.tokenLifetime).UTC(),
	}
	s.tokens = append(s.tokens, issued)
	s.mints = append(s.mints, MintEntry{InstallationID: inst.ID, Repositories: issued.Repositories, Permissions: perms, Time: now})

	response := accessTokenResponse{
		Token:               issued.Token,
		ExpiresAt:           issued.ExpiresAt,
		Permissions:         perms,
		RepositorySelection: "selected",
	}
	if body.Repositories == nil {
		response.RepositorySelection = selection(inst)
	}

	for _, name := range repoNames {
		response.Repositories = append(response.Repositories, map[string]any{
			"id": s.repos[inst.Owner+"/"+name].ID, "name": name, "full_name": inst.Owner + "/" + name,
			"owner": map[string]any{"login": inst.Owner},
		})
	}

	writeJSON(writer, http.StatusCreated, response)
}

// installationReaches reports whether the installation can reach an existing
// repository of its owner. The caller holds the lock.
func (s *Server) installationReaches(inst *Installation, name string) bool {
	if _, ok := s.repos[inst.Owner+"/"+name]; !ok {
		return false
	}

	return inst.Repositories == nil || slices.Contains(inst.Repositories, name)
}

// need is a permission an endpoint requires.
type need struct{ perm, level string }

// tokenContext is an authenticated installation token.
type tokenContext struct {
	issued IssuedToken
	owner  string
}

func (c tokenContext) hasRepo(owner, name string) bool {
	return owner == c.owner && slices.Contains(c.issued.Repositories, name)
}

func (c tokenContext) hasPermission(perm, level string) bool {
	return levelRank[c.issued.Permissions[perm]] >= levelRank[level]
}

// lookupToken finds a live installation token.
func (s *Server) lookupToken(raw string) (tokenContext, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowLocked()

	for _, issued := range s.tokens {
		if issued.Token == raw && now.Before(issued.ExpiresAt) {
			inst := s.installations[issued.InstallationID]
			if inst == nil {
				return tokenContext{}, false
			}

			return tokenContext{issued: issued, owner: inst.Owner}, true
		}
	}

	return tokenContext{}, false
}

type repoHandler func(writer http.ResponseWriter, request *http.Request, token tokenContext)

// withToken authenticates an installation token and, for repository
// endpoints, answers 403 "Resource not accessible by integration" when the
// token lacks the permission or the repository:
// https://docs.github.com/en/rest/overview/permissions-required-for-github-apps
func (s *Server) withToken(required *need, next repoHandler) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		raw, ok := bearer(request)
		if !ok {
			writeJSONError(writer, http.StatusUnauthorized, "Requires authentication")

			return
		}

		token, ok := s.lookupToken(raw)
		if !ok {
			writeJSONError(writer, http.StatusUnauthorized, "Bad credentials")

			return
		}

		if required != nil {
			owner, name := request.PathValue("owner"), request.PathValue("repo")
			reachable := token.hasRepo(owner, name) && token.hasPermission(required.perm, required.level)

			switch {
			case reachable:
			case required.level == "read" && s.isPublic(owner, name):
				// Any installation token reads public repositories, of any
				// owner, whatever its permissions and repositories.
			case owner != token.owner:
				// GitHub hides private repositories of other owners.
				writeJSONError(writer, http.StatusNotFound, "Not Found")

				return
			default:
				writeJSONError(writer, http.StatusForbidden, "Resource not accessible by integration")

				return
			}
		}

		writer.Header().Set("X-RateLimit-Limit", "5000")
		writer.Header().Set("X-RateLimit-Remaining", "4999")
		next(writer, request, token)
	}
}

// isPublic reports whether owner/name exists and is public.
func (s *Server) isPublic(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	repo := s.repos[owner+"/"+name]

	return repo != nil && !repo.Private
}

func (s *Server) handleRateLimit(writer http.ResponseWriter, _ *http.Request, _ tokenContext) {
	core := map[string]any{"limit": 5000, "remaining": 4999, "used": 1, "reset": s.Now().Add(time.Hour).Unix()}
	writeJSON(writer, http.StatusOK, map[string]any{"resources": map[string]any{"core": core}, "rate": core})
}

func (s *Server) handleGetRepo(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name := request.PathValue("owner"), request.PathValue("repo")

	s.mu.Lock()
	repo := s.repos[owner+"/"+name]
	s.mu.Unlock()

	if repo == nil {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{"id": repo.ID, "name": name, "full_name": owner + "/" + name, "private": repo.Private})
}

func (s *Server) handleIssueComment(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil || body.Body == "" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	owner, name := request.PathValue("owner"), request.PathValue("repo")

	s.mu.Lock()
	s.nextID++
	comment := map[string]any{"id": s.nextID, "body": body.Body}
	s.comments[owner+"/"+name] = append(s.comments[owner+"/"+name], comment)
	s.mu.Unlock()

	writeJSON(writer, http.StatusCreated, comment)
}

// handleListIssueComments lists a repository's issue comments a page at a
// time. Like GitHub, its next-page link names the repository by ID, in the
// /repositories/{id}/... form, not by name:
// https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api
func (s *Server) handleListIssueComments(writer http.ResponseWriter, request *http.Request, _ tokenContext) {
	owner, name := request.PathValue("owner"), request.PathValue("repo")

	perPage, page := 30, 1
	if value, err := strconv.Atoi(request.URL.Query().Get("per_page")); err == nil && value > 0 {
		perPage = min(value, 100)
	}

	if value, err := strconv.Atoi(request.URL.Query().Get("page")); err == nil && value > 0 {
		page = value
	}

	s.mu.Lock()
	repo := s.repos[owner+"/"+name]
	all := append([]map[string]any{}, s.comments[owner+"/"+name]...)
	s.mu.Unlock()

	if repo == nil {
		writeJSONError(writer, http.StatusNotFound, "Not Found")

		return
	}

	start := min((page-1)*perPage, len(all))
	end := min(start+perPage, len(all))

	if end < len(all) {
		query := request.URL.Query()
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(perPage))
		next := url.URL{Scheme: "http", Host: request.Host, Path: fmt.Sprintf("/repositories/%d/issues/comments", repo.ID), RawQuery: query.Encode()}
		writer.Header().Set("Link", `<`+next.String()+`>; rel="next"`)
	}

	writeJSON(writer, http.StatusOK, all[start:end])
}

// byRepositoryID serves a /repositories/{id}/... path as the
// /repos/{owner}/{repo}/... path of the repository with that ID.
func (s *Server) byRepositoryID(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)

		s.mu.Lock()
		repo := s.repoByIDLocked(id)
		s.mu.Unlock()

		if err != nil || repo == nil {
			writeJSONError(writer, http.StatusNotFound, "Not Found")

			return
		}

		request.SetPathValue("owner", repo.Owner)
		request.SetPathValue("repo", repo.Name)
		next(writer, request)
	}
}

// repoByIDLocked returns the repository with id, or nil. The caller holds
// the lock.
func (s *Server) repoByIDLocked(id int64) *Repository {
	for _, repo := range s.repos {
		if repo.ID == id {
			return repo
		}
	}

	return nil
}
