package github_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	appID          = 7
	installationID = 70
	owner          = "acme"
)

// clock is a settable clock for the client's caches.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// fixture is the fake GitHub behind a middleware that counts requests, keeps
// mint request bodies and lets a test answer some requests itself.
type fixture struct {
	fake  *fakegithub.Server
	api   *httptest.Server
	key   *rsa.PrivateKey
	clock *clock

	mu       sync.Mutex
	calls    map[string]int
	bodies   [][]byte
	override func(writer http.ResponseWriter, request *http.Request) bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	fake, err := fakegithub.New(t.TempDir())
	require.NoError(t, err)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	fake.CreateApp(fakegithub.App{ID: appID, Name: "app", PublicKey: &key.PublicKey})
	require.NoError(t, fake.CreateInstallation(fakegithub.Installation{
		ID: installationID, AppID: appID, Owner: owner,
		Permissions: map[string]string{"contents": "write", "issues": "write"},
	}))
	require.NoError(t, fake.CreateRepository(owner, "one", false))
	require.NoError(t, fake.CreateRepository(owner, "two", false))

	f := &fixture{
		fake:  fake,
		key:   key,
		clock: &clock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		calls: map[string]int{},
	}

	handler := fake.Handler()
	f.api = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		f.mu.Lock()
		f.calls[request.Method+" "+request.URL.Path]++

		if strings.HasSuffix(request.URL.Path, "/access_tokens") {
			body, _ := io.ReadAll(request.Body)
			f.bodies = append(f.bodies, body)
			request.Body = io.NopCloser(bytes.NewReader(body))
		}

		override := f.override
		f.mu.Unlock()

		if override != nil && override(writer, request) {
			return
		}

		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(f.api.Close)

	return f
}

func (f *fixture) setOverride(override func(writer http.ResponseWriter, request *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.override = override
}

func (f *fixture) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[call]
}

func (f *fixture) lastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.bodies) == 0 {
		return nil
	}

	return f.bodies[len(f.bodies)-1]
}

func (f *fixture) registry(metrics *github.Metrics) *github.Registry {
	return github.NewRegistry(github.Options{BaseURL: f.api.URL, Metrics: metrics, Now: f.clock.Now})
}

func (f *fixture) client(t *testing.T, metrics *github.Metrics) *github.AppClient {
	t.Helper()

	client, err := f.registry(metrics).Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "1"})
	require.NoError(t, err)

	return client
}

var appName = types.NamespacedName{Namespace: "githubgate", Name: "app"}

const listInstallations = "GET /app/installations"

func accessTokens(id int64) string {
	return "POST /app/installations/" + strconv.FormatInt(id, 10) + "/access_tokens"
}

func readRequest() github.MintRequest {
	return github.MintRequest{
		Installation: "githubgate/acme",
		Owner:        owner,
		Repositories: []string{"one"},
		Permissions:  map[string]permissions.Level{"contents": "read"},
	}
}

func requireGitHubError(t *testing.T, err error, reason github.Reason, condition string) {
	t.Helper()

	var failure *github.Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, reason, failure.Reason, failure.Message)
	require.Equal(t, condition, failure.Condition, failure.Message)
}

func otherKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return key
}

func keyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestClientFollowsChangedCredentials(t *testing.T) {
	t.Run("a changed private key is used on the next call", func(t *testing.T) {
		f := newFixture(t)
		registry := f.registry(nil)

		client, err := registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: otherKey(t), Version: "1"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionSecretInvalid)

		client, err = registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "2"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		require.NoError(t, err)
	})

	t.Run("a changed key under an unchanged version is still used", func(t *testing.T) {
		f := newFixture(t)
		registry := f.registry(nil)

		client, err := registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: otherKey(t), Version: "1"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionSecretInvalid)

		client, err = registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "1"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		require.NoError(t, err)
	})

	t.Run("a changed App ID is used on the next call", func(t *testing.T) {
		f := newFixture(t)
		registry := f.registry(nil)

		client, err := registry.Client(appName, github.Credentials{AppID: appID + 1, PrivateKey: f.key, Version: "1"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionSecretInvalid)

		client, err = registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "2"})
		require.NoError(t, err)

		_, err = client.Mint(t.Context(), readRequest())
		require.NoError(t, err)
	})

	t.Run("unchanged credentials keep the client", func(t *testing.T) {
		f := newFixture(t)
		registry := f.registry(nil)

		first, err := registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "1"})
		require.NoError(t, err)

		second, err := registry.Client(appName, github.Credentials{AppID: appID, PrivateKey: f.key, Version: "1"})
		require.NoError(t, err)
		require.Same(t, first, second)
	})

	t.Run("a missing key is refused", func(t *testing.T) {
		f := newFixture(t)

		_, err := f.registry(nil).Client(appName, github.Credentials{AppID: appID, Version: "1"})
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionSecretInvalid)
	})

	t.Run("a missing App ID is refused", func(t *testing.T) {
		f := newFixture(t)

		_, err := f.registry(nil).Client(appName, github.Credentials{PrivateKey: f.key, Version: "1"})
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionSecretInvalid)
	})
}

func TestInstallationIDLookup(t *testing.T) {
	t.Run("finds the owner on a later page, ignoring case", func(t *testing.T) {
		f := newFixture(t)

		// GitHub lists at most 100 installations per page, so the owner's
		// installation, with the highest id, is on the second page.
		for index := range 120 {
			require.NoError(t, f.fake.CreateInstallation(fakegithub.Installation{ID: int64(1000 + index), AppID: appID, Owner: "other" + strconv.Itoa(index)}))
		}

		require.NoError(t, f.fake.CreateInstallation(fakegithub.Installation{ID: 5000, AppID: appID, Owner: "MixedCase"}))

		id, err := f.client(t, nil).InstallationID(t.Context(), "mixedCASE")
		require.NoError(t, err)
		require.EqualValues(t, 5000, id)
		require.Equal(t, 2, f.count(listInstallations))
	})

	t.Run("an unknown owner is not found", func(t *testing.T) {
		f := newFixture(t)

		_, err := f.client(t, nil).InstallationID(t.Context(), "nobody")
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionInstallationNotFound)
	})

	t.Run("an id is reused for 10 minutes, then looked up again", func(t *testing.T) {
		f := newFixture(t)
		client := f.client(t, nil)

		_, err := client.InstallationID(t.Context(), owner)
		require.NoError(t, err)
		require.Equal(t, 1, f.count(listInstallations))

		f.clock.Advance(10*time.Minute - time.Second)

		_, err = client.InstallationID(t.Context(), owner)
		require.NoError(t, err)
		require.Equal(t, 1, f.count(listInstallations))

		f.clock.Advance(time.Second)

		_, err = client.InstallationID(t.Context(), owner)
		require.NoError(t, err)
		require.Equal(t, 2, f.count(listInstallations))
	})

	t.Run("a 404 from the token endpoint looks the id up again and retries", func(t *testing.T) {
		f := newFixture(t)
		client := f.client(t, nil)

		_, err := client.Mint(t.Context(), readRequest())
		require.NoError(t, err)

		// Uninstalling and reinstalling the App gives the owner a new id.
		f.fake.DeleteInstallation(installationID)
		require.NoError(t, f.fake.CreateInstallation(fakegithub.Installation{
			ID: 71, AppID: appID, Owner: owner, Permissions: map[string]string{"contents": "write"},
		}))
		f.clock.Advance(time.Minute)

		_, err = client.Mint(t.Context(), readRequest())
		require.NoError(t, err)
		require.Equal(t, 2, f.count(accessTokens(installationID)))
		require.Equal(t, 1, f.count(accessTokens(71)))
		require.Equal(t, 2, f.count(listInstallations))
	})

	t.Run("a 404 for an uninstalled App is not found", func(t *testing.T) {
		f := newFixture(t)
		client := f.client(t, nil)

		_, err := client.InstallationID(t.Context(), owner)
		require.NoError(t, err)

		f.fake.DeleteInstallation(installationID)
		f.clock.Advance(time.Minute)

		_, err = client.Mint(t.Context(), readRequest())
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionInstallationNotFound)
		require.Equal(t, 1, f.count(accessTokens(installationID)))
	})

	t.Run("a 404 within 30 seconds of a listing retries once without listing again", func(t *testing.T) {
		f := newFixture(t)
		client := f.client(t, nil)

		_, err := client.InstallationID(t.Context(), owner)
		require.NoError(t, err)

		f.fake.DeleteInstallation(installationID)

		_, err = client.Mint(t.Context(), readRequest())
		requireGitHubError(t, err, github.ReasonUnavailable, github.ConditionInstallationNotFound)
		require.Equal(t, 2, f.count(accessTokens(installationID)))
		require.Equal(t, 1, f.count(listInstallations))
	})
}
