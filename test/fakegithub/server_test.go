package fakegithub_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

type fixture struct {
	server *fakegithub.Server
	api    *httptest.Server
	key    *rsa.PrivateKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	server, err := fakegithub.New(t.TempDir())
	require.NoError(t, err)

	api := httptest.NewServer(server.Handler())
	t.Cleanup(api.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	server.CreateApp(fakegithub.App{ID: 7, Name: "app", PublicKey: &key.PublicKey})
	require.NoError(t, server.CreateInstallation(fakegithub.Installation{
		ID: 70, AppID: 7, Owner: "acme",
		Permissions: map[string]string{"contents": "write", "issues": "read"},
	}))
	require.NoError(t, server.CreateRepository("acme", "one", false))
	require.NoError(t, server.CreateRepository("acme", "two", false))

	return &fixture{server: server, api: api, key: key}
}

func (f *fixture) jwt(t *testing.T, method jwt.SigningMethod, key any, iat, exp time.Time) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(method, jwt.MapClaims{"iss": "7", "iat": iat.Unix(), "exp": exp.Unix()}).SignedString(key)
	require.NoError(t, err)

	return signed
}

func (f *fixture) goodJWT(t *testing.T) string {
	now := time.Now()

	return f.jwt(t, jwt.SigningMethodRS256, f.key, now.Add(-60*time.Second), now.Add(600*time.Second))
}

func (f *fixture) do(t *testing.T, method, path, auth string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)

		reader = bytes.NewReader(raw)
	}

	request, err := http.NewRequestWithContext(t.Context(), method, f.api.URL+path, reader)
	require.NoError(t, err)

	if auth != "" {
		request.Header.Set("Authorization", "Bearer "+auth)
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	var out map[string]any

	raw, _ := io.ReadAll(response.Body)
	_ = json.Unmarshal(raw, &out)

	return response.StatusCode, out
}

func (f *fixture) mint(t *testing.T, body any) (int, map[string]any) {
	return f.do(t, http.MethodPost, "/app/installations/70/access_tokens", f.goodJWT(t), body)
}

func (f *fixture) token(t *testing.T, body any) string {
	status, out := f.mint(t, body)
	require.Equal(t, http.StatusCreated, status, out)

	return out["token"].(string)
}

func TestAppJWTValidation(t *testing.T) {
	fix := newFixture(t)
	now := time.Now()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	hmac := fix.jwt(t, jwt.SigningMethodHS256, x509.MarshalPKCS1PublicKey(&fix.key.PublicKey), now, now.Add(time.Minute))
	rs512 := fix.jwt(t, jwt.SigningMethodRS512, fix.key, now, now.Add(time.Minute))

	tests := map[string]struct {
		token string
		want  int
	}{
		"GitHub's own recipe":       {fix.goodJWT(t), http.StatusOK},
		"expired beyond skew":       {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now.Add(-20*time.Minute), now.Add(-5*time.Minute)), http.StatusUnauthorized},
		"exp just past the skew":    {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now.Add(-5*time.Minute), now.Add(-30*time.Second)), http.StatusOK},
		"exp clearly past the skew": {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now.Add(-5*time.Minute), now.Add(-2*time.Minute)), http.StatusUnauthorized},
		"lives 11m":                 {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now, now.Add(11*time.Minute+30*time.Second)), http.StatusUnauthorized},
		"lives exactly 11m":         {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now, now.Add(11*time.Minute-10*time.Second)), http.StatusOK},
		"iat in the future":         {fix.jwt(t, jwt.SigningMethodRS256, fix.key, now.Add(2*time.Minute), now.Add(5*time.Minute)), http.StatusUnauthorized},
		"signed with another key":   {fix.jwt(t, jwt.SigningMethodRS256, otherKey, now, now.Add(time.Minute)), http.StatusUnauthorized},
		"HS256":                     {hmac, http.StatusUnauthorized},
		"RS512":                     {rs512, http.StatusUnauthorized},
		"not a JWT":                 {"garbage", http.StatusUnauthorized},
		"missing token":             {"", http.StatusUnauthorized},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			status, _ := fix.do(t, http.MethodGet, "/app", tt.token, nil)
			assert.Equal(t, tt.want, status)
		})
	}

	assert.Contains(t, fix.server.JWTs(), rs512, "every JWT received is recorded")
}

func TestAccessTokenRequests(t *testing.T) {
	fix := newFixture(t)

	tests := map[string]struct {
		body       any
		wantStatus int
		wantMsg    string
	}{
		"inaccessible repository":    {map[string]any{"repositories": []string{"missing"}}, 422, "There is at least one repository that does not exist or is not accessible to the parent installation."},
		"permission above the grant": {map[string]any{"permissions": map[string]string{"issues": "write"}}, 422, "The permissions requested are not granted to this installation."},
		"permission not granted":     {map[string]any{"permissions": map[string]string{"pull_requests": "read"}}, 422, "The permissions requested are not granted to this installation."},
		"within the grant":           {map[string]any{"repositories": []string{"one"}, "permissions": map[string]string{"issues": "read"}}, 201, ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			status, out := fix.mint(t, tt.body)
			assert.Equal(t, tt.wantStatus, status)

			if tt.wantMsg != "" {
				assert.Equal(t, tt.wantMsg, out["message"])
			}
		})
	}

	t.Run("limits", func(t *testing.T) {
		names := make([]string, 501)
		for i := range names {
			names[i] = "one"
		}

		status, _ := fix.mint(t, map[string]any{"repositories": names[:500]})
		assert.Equal(t, http.StatusCreated, status, "exactly 500 repositories")

		status, _ = fix.mint(t, map[string]any{"repositories": names})
		assert.Equal(t, http.StatusUnprocessableEntity, status, "501 repositories")
	})

	t.Run("response includes metadata read and the full grant by default", func(t *testing.T) {
		status, out := fix.mint(t, nil)
		require.Equal(t, http.StatusCreated, status)
		assert.Equal(t, map[string]any{"contents": "write", "issues": "read", "metadata": "read"}, out["permissions"])
		assert.Len(t, out["repositories"], 2)
	})

	t.Run("narrowed permissions still include metadata", func(t *testing.T) {
		_, out := fix.mint(t, map[string]any{"permissions": map[string]string{"contents": "read"}})
		assert.Equal(t, map[string]any{"contents": "read", "metadata": "read"}, out["permissions"])
	})
}

func TestInstallationListIsPaginated(t *testing.T) {
	fix := newFixture(t)

	for id := int64(71); id < 75; id++ {
		require.NoError(t, fix.server.CreateInstallation(fakegithub.Installation{ID: id, AppID: 7, Owner: "o" + strconv.Itoa(int(id))}))
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fix.api.URL+"/app/installations?per_page=2", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+fix.goodJWT(t))

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	var page []map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&page))
	assert.Len(t, page, 2)
	assert.Contains(t, response.Header.Get("Link"), `rel="next"`)
}

func TestSuspendedInstallationCannotMint(t *testing.T) {
	fix := newFixture(t)
	require.NoError(t, fix.server.SuspendInstallation(70, true))

	status, _ := fix.mint(t, nil)
	assert.Equal(t, http.StatusForbidden, status)
}

func TestInstallationTokenOnRESTEndpoints(t *testing.T) {
	fix := newFixture(t)
	full := fix.token(t, map[string]any{"repositories": []string{"one"}})
	readOnly := fix.token(t, map[string]any{"repositories": []string{"one"}, "permissions": map[string]string{"contents": "read"}})

	tests := map[string]struct {
		method, path, token string
		want                int
	}{
		"get repository":               {"GET", "/repos/acme/one", full, 200},
		"get other repository":         {"GET", "/repos/acme/two", full, 403},
		"release with contents write":  {"POST", "/repos/acme/one/releases", full, 201},
		"release with contents read":   {"POST", "/repos/acme/one/releases", readOnly, 403},
		"release in other repository":  {"POST", "/repos/acme/two/releases", full, 403},
		"comment without issues write": {"POST", "/repos/acme/one/issues/1/comments", full, 403},
		"rate limit":                   {"GET", "/rate_limit", readOnly, 200},
		"unknown token":                {"GET", "/rate_limit", "ghs_nope", 401},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var body any
			if tt.method == "POST" {
				body = map[string]string{"tag_name": "v1", "body": "hi"}
			}

			status, out := fix.do(t, tt.method, tt.path, tt.token, body)
			assert.Equal(t, tt.want, status, out)

			if tt.want == 403 {
				assert.Equal(t, "Resource not accessible by integration", out["message"])
			}
		})
	}

	t.Run("issue comment with issues write", func(t *testing.T) {
		require.NoError(t, fix.server.CreateInstallation(fakegithub.Installation{
			ID: 80, AppID: 7, Owner: "acme", Permissions: map[string]string{"issues": "write"},
		}))
		_, out := fix.do(t, "POST", "/app/installations/80/access_tokens", fix.goodJWT(t), nil)
		status, _ := fix.do(t, "POST", "/repos/acme/one/issues/1/comments", out["token"].(string), map[string]string{"body": "hi"})
		assert.Equal(t, http.StatusCreated, status)
	})

	t.Run("expired token", func(t *testing.T) {
		fix.server.AdvanceClock(2 * time.Hour)

		status, _ := fix.do(t, "GET", "/repos/acme/one", full, nil)
		assert.Equal(t, http.StatusUnauthorized, status)
	})
}

func TestControlAPI(t *testing.T) {
	fix := newFixture(t)
	control := httptest.NewServer(fix.server.ControlHandler())
	t.Cleanup(control.Close)

	post := func(path string, body any) int {
		raw, err := json.Marshal(body)
		require.NoError(t, err)

		response, err := http.Post(control.URL+path, "application/json", bytes.NewReader(raw)) //nolint:noctx // test helper
		require.NoError(t, err)

		_ = response.Body.Close()

		return response.StatusCode
	}

	get := func(path string, into any) {
		response, err := http.Get(control.URL + path) //nolint:noctx // test helper
		require.NoError(t, err)

		defer func() { _ = response.Body.Close() }()

		require.NoError(t, json.NewDecoder(response.Body).Decode(into))
	}

	pub, err := x509.MarshalPKIXPublicKey(&fix.key.PublicKey)
	require.NoError(t, err)

	pemText := "-----BEGIN PUBLIC KEY-----\n" + wrap64(pub) + "-----END PUBLIC KEY-----\n"

	require.Equal(t, 201, post("/apps", map[string]any{"id": 8, "name": "second", "public_key_pem": pemText}))
	require.Equal(t, 201, post("/installations", map[string]any{"id": 90, "app_id": 8, "owner": "beta", "permissions": map[string]string{"contents": "read"}}))
	require.Equal(t, 201, post("/repositories", map[string]any{"owner": "beta", "name": "r"}))
	require.Equal(t, 204, post("/installations/90/suspend", map[string]any{"suspended": true}))
	require.Equal(t, 204, post("/clock/advance", map[string]any{"seconds": 5}))
	assert.Equal(t, 400, post("/installations", map[string]any{"id": 91, "app_id": 999}))

	fix.token(t, map[string]any{"repositories": []string{"one"}})

	var mints []fakegithub.MintEntry

	get("/mint-log", &mints)
	require.Len(t, mints, 1)
	assert.Equal(t, []string{"one"}, mints[0].Repositories)

	var tokens []fakegithub.IssuedToken

	get("/tokens", &tokens)
	require.Len(t, tokens, 1)
	assert.True(t, strings.HasPrefix(tokens[0].Token, "ghs_"))

	var jwts []string

	get("/jwts", &jwts)
	assert.NotEmpty(t, jwts)

	status, _ := fix.do(t, http.MethodPost, "/app/installations/90/access_tokens", fix.goodJWT(t), nil)
	assert.Equal(t, http.StatusNotFound, status, "the first app does not own installation 90")

	lifetimeOf := func() time.Duration {
		_, out := fix.mint(t, map[string]any{"repositories": []string{"one"}})
		expires, err := time.Parse(time.RFC3339, out["expires_at"].(string))
		require.NoError(t, err)

		return time.Until(expires)
	}

	require.Equal(t, 204, post("/token-lifetime", map[string]any{"seconds": 60}))
	assert.Less(t, lifetimeOf(), 2*time.Minute, "tokens minted after the call live the set time")
	require.Equal(t, 204, post("/token-lifetime", map[string]any{"seconds": 0}))
	assert.Greater(t, lifetimeOf(), 50*time.Minute, "zero restores the one hour default")
}

func wrap64(raw []byte) string {
	encoded := bytes.NewBuffer(nil)
	enc := newBase64(encoded)
	_, _ = enc.Write(raw)
	_ = enc.Close()

	text := encoded.String()

	var out strings.Builder
	for len(text) > 64 {
		out.WriteString(text[:64] + "\n")
		text = text[64:]
	}

	out.WriteString(text + "\n")

	return out.String()
}

func TestGitOverHTTPS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	fix := newFixture(t)
	writeToken := fix.token(t, map[string]any{"repositories": []string{"one"}})
	readToken := fix.token(t, map[string]any{"repositories": []string{"one"}, "permissions": map[string]string{"contents": "read"}})
	otherToken := fix.token(t, map[string]any{"repositories": []string{"two"}})
	url := func(token string) string {
		return strings.Replace(fakegithub.GitURL(fix.api.URL, "acme", "one"), "http://", "http://x-access-token:"+token+"@", 1)
	}

	work := t.TempDir()
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir

		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()

		return string(out), err
	}

	out, err := git(work, "init", "--initial-branch=main", "local")
	require.NoError(t, err, out)

	local := filepath.Join(work, "local")
	require.NoError(t, os.WriteFile(filepath.Join(local, "f"), []byte("x"), 0o600))

	out, err = git(local, "add", "f")
	require.NoError(t, err, out)
	out, err = git(local, "commit", "-m", "init")
	require.NoError(t, err, out)

	out, err = git(local, "push", url(readToken), "main")
	require.Error(t, err, "push with contents read is refused: %s", out)
	assert.Contains(t, out, "403")

	out, err = git(local, "push", url(otherToken), "main")
	require.Error(t, err, "a token for another repository gets nothing")
	assert.Contains(t, out, "not found")

	out, err = git(local, "push", url(writeToken), "main")
	require.NoError(t, err, out)

	out, err = git(work, "clone", url(readToken), "clone")
	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(work, "clone", "f"))

	out, err = git(work, "clone", url(otherToken), "clone2")
	require.Error(t, err, out)

	response, err := http.Get(fakegithub.GitURL(fix.api.URL, "acme", "one") + "/info/refs?service=git-upload-pack") //nolint:noctx // test
	require.NoError(t, err)

	_ = response.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	assert.Equal(t, `Basic realm="GitHub"`, response.Header.Get("WWW-Authenticate"))
}

func newBase64(w io.Writer) io.WriteCloser { return base64.NewEncoder(base64.StdEncoding, w) }
