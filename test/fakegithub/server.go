// Package fakegithub is a test double for the parts of GitHub that githubgate
// uses: App authentication, installation tokens, a few installation-token REST
// endpoints and git smart HTTP. It copies GitHub's behaviour; each copied quirk
// carries a link to its documentation.
package fakegithub

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Permission levels in increasing order of power.
var levelRank = map[string]int{"read": 1, "write": 2, "admin": 3}

// defaultTokenLifetime is how long an installation token lives. GitHub's
// installation tokens always expire one hour after the mint (`expires_at`).
// https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
//
// SetTokenLifetime shortens it as a test-only stand-in for "one hour has
// passed": a client whose cache runs on the real clock sees a token that is
// about to expire, which moving the fake's clock cannot show.
const defaultTokenLifetime = time.Hour

// App is a registered GitHub App.
type App struct {
	ID        int64
	Name      string
	PublicKey *rsa.PublicKey
}

// Installation is an App's installation on an owner.
type Installation struct {
	ID    int64
	AppID int64
	Owner string
	// Permissions is the installation's grant.
	Permissions map[string]string
	// Repositories names the repositories the installation can reach; nil
	// means every repository of the owner.
	Repositories []string
	Suspended    bool
}

// Repository is a repository with a bare git repository on disk.
type Repository struct {
	Owner   string
	Name    string
	Private bool
}

// MintEntry records one successful installation-token request.
type MintEntry struct {
	InstallationID int64             `json:"installation_id"`
	Repositories   []string          `json:"repositories"`
	Permissions    map[string]string `json:"permissions"`
	Time           time.Time         `json:"time"`
}

// IssuedToken is an installation token the fake handed out.
type IssuedToken struct {
	Token          string            `json:"token"`
	InstallationID int64             `json:"installation_id"`
	Repositories   []string          `json:"repositories"`
	Permissions    map[string]string `json:"permissions"`
	ExpiresAt      time.Time         `json:"expires_at"`
}

// Server is the fake. Its API handler answers like api.github.com and
// github.com (git); its Control handler is for tests.
type Server struct {
	gitRoot string

	mu            sync.Mutex
	offset        time.Duration
	apps          map[int64]*App
	installations map[int64]*Installation
	repos         map[string]*Repository
	mints         []MintEntry
	tokens        []IssuedToken
	tokenLifetime time.Duration
	jwts          []string
	signed        map[string]bool
	releases      map[string]*Release
	pulls         map[string][]PullRequest
	nextID        int64
	users         *userState
}

// New returns a Server that keeps bare repositories under gitRoot (a new
// temporary directory when empty).
func New(gitRoot string) (*Server, error) {
	if gitRoot == "" {
		dir, err := os.MkdirTemp("", "fakegithub-git-")
		if err != nil {
			return nil, err
		}

		gitRoot = dir
	}

	return &Server{
		gitRoot:       gitRoot,
		apps:          map[int64]*App{},
		installations: map[int64]*Installation{},
		repos:         map[string]*Repository{},
		tokenLifetime: defaultTokenLifetime,
		signed:        map[string]bool{},
		releases:      map[string]*Release{},
		pulls:         map[string][]PullRequest{},
		nextID:        100,
		users:         newUserState(),
	}, nil
}

// Now is the fake's clock: real time plus the offset set by AdvanceClock.
func (s *Server) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.nowLocked()
}

func (s *Server) nowLocked() time.Time { return time.Now().Add(s.offset) }

// SetTokenLifetime sets the lifetime of the tokens minted from now on; a value
// of zero or less restores the default of one hour.
func (s *Server) SetTokenLifetime(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d <= 0 {
		d = defaultTokenLifetime
	}

	s.tokenLifetime = d
}

// AdvanceClock moves the fake's clock forward, for example to expire tokens.
func (s *Server) AdvanceClock(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.offset += d
}

// CreateApp registers an App that signs its JWTs with the private key of key.
func (s *Server) CreateApp(app App) {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := app
	s.apps[app.ID] = &copied
}

// CreateInstallation registers an installation.
func (s *Server) CreateInstallation(inst Installation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.apps[inst.AppID]; !ok {
		return fmt.Errorf("app %d does not exist", inst.AppID)
	}

	copied := inst
	s.installations[inst.ID] = &copied

	return nil
}

// SuspendInstallation suspends or resumes an installation.
func (s *Server) SuspendInstallation(id int64, suspended bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.installations[id]
	if !ok {
		return fmt.Errorf("installation %d does not exist", id)
	}

	inst.Suspended = suspended

	return nil
}

// DeleteInstallation removes an installation, as uninstalling the App does.
// Reinstalling it later gives a new id.
func (s *Server) DeleteInstallation(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.installations, id)
}

// CreateRepository creates a repository and its bare git repository, whose
// default branch is main. Repositories are private unless public is set.
func (s *Server) CreateRepository(owner, name string, public bool) error {
	if !validName(owner) || !validName(name) {
		return errors.New("invalid owner or repository name")
	}

	dir := filepath.Join(s.gitRoot, owner, name+".git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.repos[owner+"/"+name] = &Repository{Owner: owner, Name: name, Private: !public}

	return nil
}

// GitURL is the clone URL of a repository on the API server at baseURL.
func GitURL(baseURL, owner, name string) string {
	return strings.TrimSuffix(baseURL, "/") + "/" + owner + "/" + name + ".git"
}

// MintLog returns every successful installation-token request.
func (s *Server) MintLog() []MintEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]MintEntry(nil), s.mints...)
}

// Tokens returns every installation token issued so far.
func (s *Server) Tokens() []IssuedToken {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]IssuedToken(nil), s.tokens...)
}

// JWTs returns every App JWT received, valid or not, for leak searches.
func (s *Server) JWTs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.jwts...)
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}

	return !strings.ContainsAny(name, "/\\\x00 ")
}

func newToken() string {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}

	return "ghs_" + hex.EncodeToString(raw)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// Handler serves the GitHub API and git over HTTPS on one handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", s.handleApp)
	mux.HandleFunc("GET /app/installations", s.handleListInstallations)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.handleAccessTokens)
	mux.HandleFunc("GET /rate_limit", s.withToken(nil, s.handleRateLimit))
	mux.HandleFunc("GET /repos/{owner}/{repo}", s.withToken(&need{perm: "metadata", level: "read"}, s.handleGetRepo))
	contentsWrite := &need{perm: "contents", level: "write"}
	contentsRead := &need{perm: "contents", level: "read"}

	mux.HandleFunc("POST /repos/{owner}/{repo}/releases", s.withToken(contentsWrite, s.handleCreateRelease))
	mux.HandleFunc("POST /repos/{owner}/{repo}/releases/{id}/assets", s.withToken(contentsWrite, s.handleUploadReleaseAsset))
	mux.HandleFunc("GET /repos/{owner}/{repo}/tarball/{ref...}", s.withToken(contentsRead, s.handleArchive("tar")))
	mux.HandleFunc("GET /repos/{owner}/{repo}/zipball/{ref...}", s.withToken(contentsRead, s.handleArchive("zip")))
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/tags/{tag}", s.withToken(contentsRead, s.handleGetReleaseByTag))
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/assets/{id}", s.withToken(contentsRead, s.handleDownloadReleaseAsset))
	mux.HandleFunc("GET /_storage_asset/{owner}/{repo}/{id}", s.handleAssetStorage)
	mux.HandleFunc("GET /_storage/{owner}/{repo}/{ref...}", s.handleStorage)
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/blobs", s.withToken(contentsWrite, s.handleCreateBlob))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", s.withToken(contentsRead, s.handleGetBlob))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/trees", s.withToken(contentsWrite, s.handleCreateTree))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/commits", s.withToken(contentsWrite, s.handleCreateCommit))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}", s.withToken(contentsRead, s.handleGetCommit))
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs", s.withToken(contentsWrite, s.handleCreateRef))
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/git/refs/{ref...}", s.withToken(contentsWrite, s.handleUpdateRef))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/{ref...}", s.withToken(contentsRead, s.handleGetRef))
	mux.HandleFunc("POST /graphql", s.withToken(nil, s.handleGraphQL))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", s.withToken(&need{perm: "issues", level: "write"}, s.handleIssueComment))
	mux.HandleFunc("POST /login/device/code", s.handleDeviceCode)
	mux.HandleFunc("POST /login/oauth/access_token", s.handleOAuthAccessToken)
	mux.HandleFunc("GET /user", s.userHandler(s.handleUser))
	mux.HandleFunc("GET /user/ssh_signing_keys", s.userHandler(s.handleListSSHSigningKeys))
	mux.HandleFunc("POST /user/ssh_signing_keys", s.userHandler(s.handleCreateSSHSigningKey))
	mux.HandleFunc("GET /user/ssh_signing_keys/{id}", s.userHandler(s.handleGetSSHSigningKey))
	mux.HandleFunc("DELETE /user/ssh_signing_keys/{id}", s.userHandler(s.handleDeleteSSHSigningKey))
	mux.HandleFunc("/{owner}/{repo}/", s.handleGit)

	return mux
}
