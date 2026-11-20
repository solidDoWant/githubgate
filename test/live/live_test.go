//go:build live

// Package live tests githubgate against github.com with the owner's test App.
// Only the operator runs it, by hand (make test-live). It skips without its
// inputs. Nothing here is ever run in CI or by an agent.
package live_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/server"
)

const (
	adminNamespace  = "githubgate"
	tenantNamespace = "ci-a"
	buildAccount    = "ci-build"
	releaseAccount  = "ci-release"
	proxyAccount    = "ci-proxy"
	pushAccount     = "ci-push"
	apiRoot         = "https://api.github.com"
	clientBinary    = "githubgate-client"
)

// transcript is everything the test prints, token requests' answers and the
// output of every command included. The last step searches it for each token.
type transcript struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	tokens map[string]bool
}

func (tr *transcript) Write(data []byte) (int, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return tr.buffer.Write(data)
}

func (tr *transcript) addToken(token string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	tr.tokens[token] = true
}

// linesContaining returns the transcript's lines that contain substring.
func (tr *transcript) linesContaining(substring string) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	var lines []string

	for _, line := range strings.Split(tr.buffer.String(), "\n") {
		if strings.Contains(line, substring) {
			lines = append(lines, line)
		}
	}

	return lines
}

func (tr *transcript) printf(t *testing.T, format string, args ...any) {
	t.Helper()

	line := fmt.Sprintf(format, args...)
	_, _ = tr.Write([]byte(line + "\n"))
	t.Log(line)
}

type liveEnv struct {
	appID   int64
	keyPEM  []byte
	owner   string
	repo    string
	srv     *httptest.Server
	caFile  string
	bin     string
	home    string
	log     *transcript
	tmp     string
	gitPath string
	store   *model.Store
	api     *server.Server
	// proxyAudit holds only the in-process proxy's audit lines, apart from
	// the transcript, so a failure can print exactly what the proxy logged.
	proxyAudit *transcript
}

// recordingAuthenticator takes the caller from the credential "ns/sa", so the
// test chooses the identity without a Kubernetes API. It reads the schemes
// the real authenticator does: Bearer and token (gh) and the Basic password
// (git).
type recordingAuthenticator struct{}

func (recordingAuthenticator) Authenticate(_ context.Context, request *http.Request) (auth.Identity, error) {
	value, ok := credentialFrom(request)
	if !ok {
		return auth.Identity{}, auth.ErrNoCredential
	}

	namespace, name, ok := strings.Cut(value, "/")
	if !ok {
		return auth.Identity{}, auth.ErrInvalidCredential
	}

	return auth.Identity{Namespace: namespace, ServiceAccount: name, Pod: "live"}, nil
}

// credentialFrom returns the credential of the request's one Authorization
// header, or false when there is none or its scheme is unknown.
func credentialFrom(request *http.Request) (string, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	scheme, credential, _ := strings.Cut(values[0], " ")
	switch {
	case strings.EqualFold(scheme, "Bearer"), strings.EqualFold(scheme, "token"):
		return strings.TrimSpace(credential), true
	case strings.EqualFold(scheme, "Basic"):
		_, password, ok := request.BasicAuth()
		return password, ok
	default:
		return "", false
	}
}

// tokenRecorder remembers every token the token API hands out.
func tokenRecorder(next http.Handler, log *transcript) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, request)

		var body struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &body) == nil && body.Token != "" {
			log.addToken(body.Token)
		}

		for key, values := range recorder.Header() {
			writer.Header()[key] = values
		}

		writer.WriteHeader(recorder.Code)
		_, _ = writer.Write(recorder.Body.Bytes())
	})
}

func rule(name, repo string, set map[string]githubgatev1alpha1.Level) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(repo)},
		Permissions:     set,
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}

func proxyRule(name, repo string, set map[string]githubgatev1alpha1.Level, mode githubgatev1alpha1.ProxyMode) githubgatev1alpha1.AccessRule {
	mode.Enabled = true

	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(repo)},
		Permissions:     set,
		Proxy:           &mode,
	}
}

func policy(account string, rules ...githubgatev1alpha1.AccessRule) githubgatev1alpha1.GitHubAccessPolicy {
	return githubgatev1alpha1.GitHubAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: account},
		Spec:       githubgatev1alpha1.GitHubAccessPolicySpec{ServiceAccounts: []string{account}, Rules: rules},
	}
}

func setup(t *testing.T) *liveEnv {
	t.Helper()

	keyFile := os.Getenv("GITHUBGATE_LIVE_KEY_FILE")
	idText := os.Getenv("GITHUBGATE_LIVE_APP_ID")
	owner := os.Getenv("GITHUBGATE_LIVE_OWNER")
	repo := os.Getenv("GITHUBGATE_LIVE_REPO")

	if keyFile == "" || idText == "" || owner == "" || repo == "" {
		t.Skip("the live test needs GITHUBGATE_LIVE_APP_ID, GITHUBGATE_LIVE_KEY_FILE, GITHUBGATE_LIVE_OWNER and GITHUBGATE_LIVE_REPO; skipping")
	}

	appID, err := strconv.ParseInt(idText, 10, 64)
	require.NoError(t, err, "GITHUBGATE_LIVE_APP_ID must be a number")

	keyPEM, err := os.ReadFile(keyFile)
	require.NoError(t, err)

	for _, tool := range []string{"git", "gh", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; skipping", tool)
		}
	}

	live := &liveEnv{
		appID: appID, keyPEM: keyPEM, owner: owner, repo: repo,
		log: &transcript{tokens: map[string]bool{}}, tmp: t.TempDir(),
		proxyAudit: &transcript{tokens: map[string]bool{}},
	}

	// The token API runs in-process on a snapshot built from the test's own
	// objects, against the real github.com.
	id := appID
	inputs := model.Inputs{
		AdminNamespaces: []string{adminNamespace},
		Apps: []githubgatev1alpha1.GitHubApp{{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "app"},
			Spec: githubgatev1alpha1.GitHubAppSpec{
				AppID:               &id,
				PrivateKeySecretRef: githubgatev1alpha1.SecretKeyReference{Name: "app-key"},
			},
		}},
		Installations: []githubgatev1alpha1.GitHubInstallation{{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "main"},
			Spec: githubgatev1alpha1.GitHubInstallationSpec{
				AppRef: githubgatev1alpha1.ObjectReference{Name: "app"},
				Owner:  owner,
				Ceiling: []githubgatev1alpha1.PermissionSet{{
					Repositories: []githubgatev1alpha1.Repository{githubgatev1alpha1.Repository(repo)},
					Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "write", "pull_requests": "write"},
				}},
				AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
			},
		}},
		Policies: []githubgatev1alpha1.GitHubAccessPolicy{
			policy(buildAccount, rule("read", repo, map[string]githubgatev1alpha1.Level{"contents": "read"})),
			policy(releaseAccount, rule("write", repo, map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "write"})),
			policy(proxyAccount, proxyRule("proxy", repo, map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "write", "pull_requests": "write"},
				githubgatev1alpha1.ProxyMode{
					DenyOperations: []githubgatev1alpha1.OperationPattern{"repos/create-release"},
					AllowMutations: []githubgatev1alpha1.MutationPattern{"createPullRequest"},
				})),
			policy(pushAccount, proxyRule("push", repo, map[string]githubgatev1alpha1.Level{"contents": "write"},
				githubgatev1alpha1.ProxyMode{PushRefs: []githubgatev1alpha1.RefPattern{"refs/heads/agent/**"}})),
		},
		Secrets: []corev1.Secret{{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "app-key", ResourceVersion: "1"},
			Data:       map[string][]byte{"private-key.pem": keyPEM},
		}},
		Namespaces: []corev1.Namespace{
			{ObjectMeta: metav1.ObjectMeta{Name: adminNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace}},
		},
	}

	store := &model.Store{}
	store.Swap(model.Build(inputs))

	tokenAPI := server.New(server.Options{
		Authenticator: recordingAuthenticator{},
		Store:         store,
		GitHub:        github.NewRegistry(github.Options{}),
		Audit:         slog.New(slog.NewJSONHandler(live.log, nil)),
	})

	live.store, live.api = store, tokenAPI
	live.srv = httptest.NewUnstartedServer(tokenRecorder(tokenAPI.Handler(), live.log))
	live.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	live.srv.StartTLS()
	t.Cleanup(live.srv.Close)

	live.caFile = filepath.Join(live.tmp, "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: live.srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(live.caFile, caPEM, 0o600))

	for _, account := range []string{buildAccount, releaseAccount, proxyAccount, pushAccount} {
		require.NoError(t, os.WriteFile(live.tokenFile(account), []byte(tenantNamespace+"/"+account), 0o600))
	}

	// The real client, built from this checkout.
	live.bin = filepath.Join(live.tmp, "bin")
	require.NoError(t, os.MkdirAll(live.bin, 0o700))

	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(live.bin, clientBinary), "github.com/solidDoWant/githubgate/cmd/githubgate-client")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	live.home = filepath.Join(live.tmp, "home")
	require.NoError(t, os.MkdirAll(live.home, 0o700))

	return live
}

func (l *liveEnv) tokenFile(account string) string {
	return filepath.Join(l.tmp, account+".token")
}

// env is the environment of a command that runs as one ServiceAccount.
func (l *liveEnv) env(account string) []string {
	return append(os.Environ(),
		"PATH="+l.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+l.home,
		"XDG_CONFIG_HOME="+filepath.Join(l.home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=githubgate live test", "GIT_AUTHOR_EMAIL=live-test@example.invalid",
		"GIT_COMMITTER_NAME=githubgate live test", "GIT_COMMITTER_EMAIL=live-test@example.invalid",
		"GITHUBGATE_URL="+l.srv.URL,
		"GITHUBGATE_CA_FILE="+l.caFile,
		"GITHUBGATE_TOKEN_FILE="+l.tokenFile(account),
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_PROMPT_DISABLED=1",
	)
}

// run runs a command as a ServiceAccount, records its output and returns it.
func (l *liveEnv) run(t *testing.T, account, dir string, name string, args ...string) (string, error) {
	t.Helper()

	// exec resolves a bare name with the test process's PATH, not the
	// command's, so the client built into the bin directory is named by path.
	if name == clientBinary {
		name = filepath.Join(l.bin, clientBinary)
	}

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), name, args...)
	cmd.Dir = dir
	cmd.Env = l.env(account)

	out, err := cmd.CombinedOutput()
	l.log.printf(t, "$ %s %s\n%s", name, strings.Join(args, " "), out)

	return string(out), err
}

// tokenResult is the answer of the token API.
type tokenResult struct {
	status       int
	token        string
	repositories []string
	permissions  map[string]string
	message      string
}

func (l *liveEnv) requestToken(t *testing.T, account string, body any) tokenResult {
	t.Helper()

	data, err := json.Marshal(body)
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodPost, l.srv.URL+"/v1/token", bytes.NewReader(data))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+tenantNamespace+"/"+account)

	httpClient := l.srv.Client()
	response, err := httpClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	var parsed struct {
		Token        string            `json:"token"`
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
		Message      string            `json:"message"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))

	if parsed.Token != "" {
		l.log.addToken(parsed.Token)
	}

	l.log.printf(t, "token request as %s: status %d, repositories %v, permissions %v, message %q",
		account, response.StatusCode, parsed.Repositories, parsed.Permissions, parsed.Message)

	return tokenResult{status: response.StatusCode, token: parsed.Token, repositories: parsed.Repositories, permissions: parsed.Permissions, message: parsed.Message}
}

// github calls GitHub's REST API with a token and returns the status and body.
func (l *liveEnv) github(t *testing.T, token, method, path string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)

		reader = bytes.NewReader(data)
	}

	request, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), method, apiRoot+path, reader)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	l.log.printf(t, "GitHub %s %s: status %d", method, path, response.StatusCode)

	return response.StatusCode, raw
}

func unique(t *testing.T) string {
	t.Helper()

	random := make([]byte, 4)
	_, err := rand.Read(random)
	require.NoError(t, err)

	sum := sha256.Sum256([]byte(t.Name()))

	return "githubgate-live-" + hex.EncodeToString(sum[:3]) + "-" + hex.EncodeToString(random)
}

func TestLiveAgainstGitHub(t *testing.T) {
	live := setup(t)
	repoPath := "/repos/" + live.owner + "/" + live.repo
	cloneURL := "https://github.com/" + live.owner + "/" + live.repo + ".git"

	t.Run("a token has exactly the policy's repository and permissions", func(t *testing.T) {
		got := live.requestToken(t, buildAccount, map[string]any{"owner": live.owner, "repositories": []string{live.repo}})
		require.Equal(t, http.StatusOK, got.status, got.message)
		assert.Equal(t, []string{live.repo}, got.repositories)
		assert.Equal(t, map[string]string{"contents": "read", "metadata": "read"}, got.permissions)

		status, body := live.github(t, got.token, http.MethodGet, "/installation/repositories", nil)
		require.Equal(t, http.StatusOK, status)

		var listed struct {
			Repositories []struct {
				Name string `json:"name"`
			} `json:"repositories"`
		}
		require.NoError(t, json.Unmarshal(body, &listed))
		require.Len(t, listed.Repositories, 1)
		assert.Equal(t, live.repo, listed.Repositories[0].Name)
	})

	t.Run("a permission or level the policy does not give is refused", func(t *testing.T) {
		got := live.requestToken(t, buildAccount, map[string]any{
			"owner": live.owner, "repositories": []string{live.repo}, "permissions": map[string]string{"contents": "write"},
		})
		assert.Equal(t, http.StatusForbidden, got.status)
		assert.Contains(t, got.message, "contents")
		assert.Empty(t, got.token)

		got = live.requestToken(t, buildAccount, map[string]any{
			"owner": live.owner, "repositories": []string{live.repo}, "permissions": map[string]string{"issues": "read"},
		})
		assert.Equal(t, http.StatusForbidden, got.status)
		assert.Contains(t, got.message, "issues")
	})

	t.Run("the build account can clone but not release and the release account can", func(t *testing.T) {
		build := live.requestToken(t, buildAccount, map[string]any{"owner": live.owner, "repositories": []string{live.repo}})
		require.Equal(t, http.StatusOK, build.status, build.message)

		tag := unique(t)
		release := map[string]any{"tag_name": tag, "name": tag}

		status, _ := live.github(t, build.token, http.MethodGet, repoPath, nil)
		assert.Equal(t, http.StatusOK, status, "a contents:read token reads the repository")

		status, _ = live.github(t, build.token, http.MethodPost, repoPath+"/releases", release)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, status, "a contents:read token cannot create a release")

		releaser := live.requestToken(t, releaseAccount, map[string]any{
			"owner": live.owner, "repositories": []string{live.repo}, "permissions": map[string]string{"contents": "write"},
		})
		require.Equal(t, http.StatusOK, releaser.status, releaser.message)

		// Registered before the release exists, so a failed run still
		// removes whatever was created.
		t.Cleanup(func() {
			fresh := live.requestToken(t, releaseAccount, map[string]any{
				"owner": live.owner, "repositories": []string{live.repo}, "permissions": map[string]string{"contents": "write"},
			})
			if fresh.token == "" {
				t.Errorf("could not get a token to remove release %s and its tag", tag)

				return
			}

			status, body := live.github(t, fresh.token, http.MethodGet, repoPath+"/releases/tags/"+tag, nil)
			if status == http.StatusOK {
				var found struct {
					ID int64 `json:"id"`
				}
				if json.Unmarshal(body, &found) == nil {
					live.github(t, fresh.token, http.MethodDelete, repoPath+"/releases/"+strconv.FormatInt(found.ID, 10), nil)
				}
			}

			live.github(t, fresh.token, http.MethodDelete, repoPath+"/git/refs/tags/"+tag, nil)
		})

		status, body := live.github(t, releaser.token, http.MethodPost, repoPath+"/releases", release)
		require.Equal(t, http.StatusCreated, status, string(body))
	})

	t.Run("git clone, fetch and push work after configure with no other setup", func(t *testing.T) {
		_, err := live.run(t, buildAccount, live.tmp, clientBinary, "configure")
		require.NoError(t, err)

		work := filepath.Join(live.tmp, "work")
		_, err = live.run(t, buildAccount, live.tmp, "git", "clone", cloneURL, work)
		require.NoError(t, err)

		_, err = live.run(t, buildAccount, work, "git", "fetch", "origin")
		require.NoError(t, err)

		// The build account's contents:read cannot push.
		branch := unique(t)
		_, err = live.run(t, buildAccount, work, "git", "checkout", "-b", branch)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(work, branch+".txt"), []byte("githubgate live test\n"), 0o600))
		_, err = live.run(t, buildAccount, work, "git", "add", branch+".txt")
		require.NoError(t, err)
		_, err = live.run(t, buildAccount, work, "git", "commit", "-m", "githubgate live test")
		require.NoError(t, err)

		_, err = live.run(t, buildAccount, work, "git", "push", "origin", branch)
		require.Error(t, err, "a contents:read token cannot push")

		t.Cleanup(func() {
			_, _ = live.run(t, releaseAccount, work, "git", "push", "origin", "--delete", branch)
		})

		_, err = live.run(t, releaseAccount, work, "git", "push", "origin", branch)
		require.NoError(t, err)
	})

	t.Run("gh issue comment works through the client", func(t *testing.T) {
		title := unique(t)
		out, err := live.run(t, releaseAccount, live.tmp, clientBinary, "gh", "--repo", live.owner+"/"+live.repo, "--",
			"issue", "create", "--repo", live.owner+"/"+live.repo, "--title", title, "--body", "githubgate live test")
		require.NoError(t, err)

		lines := strings.Fields(strings.TrimSpace(out))
		require.NotEmpty(t, lines)
		issue := lines[len(lines)-1]

		// Issues cannot be deleted, so the test closes the one it opened.
		t.Cleanup(func() {
			_, _ = live.run(t, releaseAccount, live.tmp, clientBinary, "gh", "--repo", live.owner+"/"+live.repo, "--",
				"issue", "close", issue)
		})

		_, err = live.run(t, releaseAccount, live.tmp, clientBinary, "gh", "--repo", live.owner+"/"+live.repo, "--",
			"issue", "comment", issue, "--body", "githubgate live test comment")
		require.NoError(t, err)
	})

	t.Cleanup(func() {
		live.log.mu.Lock()
		defer live.log.mu.Unlock()

		output := live.log.buffer.String()
		assert.NotEmpty(t, live.log.tokens, "the run saw no token")

		for token := range live.log.tokens {
			assert.NotContains(t, output, token, "a token appears in the test's output")
		}

		assert.NotContains(t, output, "PRIVATE KEY", "a private key appears in the test's output")
	})
}
