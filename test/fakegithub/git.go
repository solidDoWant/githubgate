package fakegithub

import (
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"strings"
)

// handleGit serves git smart HTTP for `/{owner}/{repo}.git/...` with Basic
// auth, answering like github.com:
//   - no credential: 401 with WWW-Authenticate: Basic realm="GitHub"
//   - a token without access to the repository: 404 "Repository not found"
//     (GitHub hides private repositories)
//   - a push without contents: write: 403
//
// https://docs.github.com/en/get-started/git-basics/about-remote-repositories
func (s *Server) handleGit(writer http.ResponseWriter, request *http.Request) {
	owner := request.PathValue("owner")
	repoDir, ok := strings.CutSuffix(request.PathValue("repo"), ".git")

	if !ok || !validName(owner) || !validName(repoDir) {
		http.NotFound(writer, request)

		return
	}

	username, password, hasAuth := request.BasicAuth()
	if !hasAuth || password == "" {
		writer.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(writer, "Authentication required", http.StatusUnauthorized)

		return
	}

	// Only the literal user name x-access-token works with an installation
	// token; any other user name is treated as a failed password login:
	// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation#using-an-installation-access-token-to-authenticate-with-git
	token, ok := s.lookupToken(password)
	if !ok || username != "x-access-token" {
		writer.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(writer, "Invalid username or token. Password authentication is not supported for Git operations.", http.StatusUnauthorized)

		return
	}

	s.mu.Lock()
	_, exists := s.repos[owner+"/"+repoDir]
	s.mu.Unlock()

	if !exists || !token.hasRepo(owner, repoDir) || !token.hasPermission("contents", "read") {
		http.Error(writer, "Repository not found.", http.StatusNotFound)

		return
	}

	if isPush(request) && !token.hasPermission("contents", "write") {
		http.Error(writer, "Permission to "+owner+"/"+repoDir+".git denied to the installation token.", http.StatusForbidden)

		return
	}

	backend, err := gitHTTPBackend()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)

		return
	}

	handler := &cgi.Handler{
		Path: backend,
		Dir:  s.gitRoot,
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.gitRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=x-access-token",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.receivepack",
			"GIT_CONFIG_VALUE_0=true",
		},
	}
	handler.ServeHTTP(writer, request)
}

func isPush(request *http.Request) bool {
	return request.URL.Query().Get("service") == "git-receive-pack" ||
		strings.HasSuffix(request.URL.Path, "/git-receive-pack")
}

func gitHTTPBackend() (string, error) {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", err
	}

	return filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"), nil
}
