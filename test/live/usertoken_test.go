//go:build live

package live_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	liveClientIDEnv = "GITHUBGATE_LIVE_CLIENT_ID"

	listSigningKeys  = "users/list-ssh-signing-keys-for-authenticated-user"
	createSigningKey = "users/create-ssh-signing-key-for-authenticated-user"
	deleteSigningKey = "users/delete-ssh-signing-key-for-authenticated-user"
	getUser          = "users/get-authenticated"

	signingKeyTitlePrefix = "githubgate-live-"
	userAccount           = "ci-user"
)

// heldUserToken is the user access token the test holds in memory, as the
// leader's Secret holds it in a cluster.
type heldUserToken struct {
	mu    sync.Mutex
	token string
}

func (h *heldUserToken) UserToken(_ context.Context, _ *model.UserAuthorization) (string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.token, "no token held", nil
}

func (h *heldUserToken) set(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.token = token
}

// userAPI calls api.github.com through the in-process proxy as a caller whose
// only access is a user rule.
type userAPI struct {
	client *http.Client
}

func (a userAPI) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}

		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, apiRoot+path, reader)
	if err != nil {
		return 0, nil, err
	}

	request.Header.Set("Authorization", "Bearer "+tenantNamespace+"/"+userAccount)
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := a.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))

	return response.StatusCode, data, err
}

type signingKey struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func (a userAPI) listKeys(ctx context.Context) ([]signingKey, error) {
	status, data, err := a.do(ctx, http.MethodGet, "/user/ssh_signing_keys?per_page=100", nil)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK {
		return nil, &statusError{status: status}
	}

	var keys []signingKey

	return keys, json.Unmarshal(data, &keys)
}

type statusError struct{ status int }

func (e *statusError) Error() string { return "GitHub answered HTTP " + strconv.Itoa(e.status) }

// newSigningPublicKey makes a throwaway ed25519 key with ssh-keygen and
// returns its public half.
func newSigningPublicKey(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "key")
	out, err := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput()
	require.NoError(t, err, string(out))

	public, err := os.ReadFile(path + ".pub")
	require.NoError(t, err)

	return strings.TrimSpace(string(public))
}

// startUserProxy starts the in-process proxy with a user rule for the three
// signing-key operations and a read of the signed-in user, and returns a
// client that reaches GitHub only through it.
func (l *liveEnv) startUserProxy(t *testing.T, held *heldUserToken) userAPI {
	t.Helper()

	view := &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: adminNamespace, Name: "owner-user"},
		Login:          l.owner, Resolved: true, Reason: model.ReasonReady,
	}
	l.userTokens = held
	l.userRules = map[string][]*model.Rule{userAccount: {{
		Policy: types.NamespacedName{Namespace: tenantNamespace, Name: userAccount}, Name: "signing-keys",
		User: &model.UserRule{
			Authorization: view.NamespacedName, View: view,
			Operations: []string{getUser, listSigningKeys, createSigningKey, deleteSigningKey},
			Accepted:   true, Reason: model.ReasonAccepted,
		},
	}}}

	tunnel := l.startProxy(t)

	tunnelURL, err := url.Parse(tunnel.tunnelURL)
	require.NoError(t, err)

	caPEM, err := os.ReadFile(tunnel.caFile)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	return userAPI{client: &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(tunnelURL),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}}
}

// authorizeAsOwner runs GitHub's device flow with the test App and waits,
// printing the user code, until the owner approves it. The device code and
// the tokens are never printed.
func authorizeAsOwner(t *testing.T, live *liveEnv, config *oauth2.Config) *oauth2.Token {
	t.Helper()

	device, err := config.DeviceAuth(t.Context())
	require.NoError(t, err)

	live.log.printf(t, "Signed in to GitHub as %s, open %s and enter the code %s (expires %s)",
		live.owner, device.VerificationURI, device.UserCode, device.Expiry.Format(time.RFC3339))

	token, err := config.DeviceAccessToken(t.Context(), device)
	require.NoError(t, err, "the device flow did not finish")

	return token
}

func TestUserAccessTokenSigningKeyLifecycleOnGitHub(t *testing.T) {
	clientID := os.Getenv(liveClientIDEnv)
	if clientID == "" {
		t.Skip("the user token test needs " + liveClientIDEnv + " besides the other live inputs; skipping")
	}

	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed; skipping")
	}

	live := setup(t)
	held := &heldUserToken{}
	api := live.startUserProxy(t, held)

	config := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: "https://github.com/login/device/code",
			TokenURL:      "https://github.com/login/oauth/access_token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}

	token := authorizeAsOwner(t, live, config)
	require.NotEmpty(t, token.RefreshToken, "turn on user-to-server token expiration in the App's settings")
	require.False(t, token.Expiry.IsZero(), "turn on user-to-server token expiration in the App's settings")

	live.log.addToken(token.AccessToken)
	live.log.addToken(token.RefreshToken)
	held.set(token.AccessToken)

	// Delete every key this run added, even when a step fails. It runs with a
	// context the test's end doesn't cancel.
	title := signingKeyTitlePrefix + strconv.FormatInt(time.Now().UnixNano(), 36)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		keys, err := api.listKeys(ctx)
		if err != nil {
			t.Errorf("could not list signing keys to clean up; remove keys titled %q by hand: %v", title, err)

			return
		}

		for _, key := range keys {
			if key.Title != title {
				continue
			}

			status, _, err := api.do(ctx, http.MethodDelete, "/user/ssh_signing_keys/"+strconv.FormatInt(key.ID, 10), nil)
			if err != nil || status != http.StatusNoContent {
				t.Errorf("could not delete signing key %d titled %q; remove it by hand (status %d, error %v)", key.ID, title, status, err)
			}
		}
	})

	// The token belongs to the owner.
	status, data, err := api.do(t.Context(), http.MethodGet, "/user", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	var user struct {
		Login string `json:"login"`
	}

	require.NoError(t, json.Unmarshal(data, &user))
	assert.True(t, strings.EqualFold(live.owner, user.Login), "the token belongs to %q, not the owner", user.Login)

	// Add a key through the proxy and see it on the account.
	status, data, err = api.do(t.Context(), http.MethodPost, "/user/ssh_signing_keys",
		map[string]string{"title": title, "key": newSigningPublicKey(t)})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(data))

	var created signingKey

	require.NoError(t, json.Unmarshal(data, &created))
	require.NotZero(t, created.ID)

	keys, err := api.listKeys(t.Context())
	require.NoError(t, err)
	assert.Contains(t, keys, signingKey{ID: created.ID, Title: title})

	// Delete it and see it gone.
	status, _, err = api.do(t.Context(), http.MethodDelete, "/user/ssh_signing_keys/"+strconv.FormatInt(created.ID, 10), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, status)

	keys, err = api.listKeys(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, keys, signingKey{ID: created.ID, Title: title})

	// One refresh grant, called once as the controller calls it, and the new
	// access token works.
	refreshed, err := config.TokenSource(t.Context(), &oauth2.Token{RefreshToken: token.RefreshToken}).Token()
	require.NoError(t, err, "GitHub refused the refresh token")
	require.NotEmpty(t, refreshed.AccessToken)
	assert.NotEqual(t, token.AccessToken, refreshed.AccessToken)
	assert.NotEqual(t, token.RefreshToken, refreshed.RefreshToken, "a refresh token is single-use")

	live.log.addToken(refreshed.AccessToken)
	live.log.addToken(refreshed.RefreshToken)
	held.set(refreshed.AccessToken)

	status, _, err = api.do(t.Context(), http.MethodGet, "/user", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status, "the refreshed access token was refused")

	// Nothing the test printed or the proxy logged holds a token.
	live.log.mu.Lock()
	printed := live.log.buffer.String()
	live.log.mu.Unlock()

	for secret := range live.log.tokens {
		assert.NotContains(t, printed, secret, "a token appeared in the output")
	}
}
