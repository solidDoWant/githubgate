package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"io"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/server"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	adminNamespace  = "githubgate"
	tenantNamespace = "ci-a"
	owner           = "solidDoWant"
	appID           = 42
	installationID  = 7
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
})

// headerAuthenticator takes the caller from "Authorization: Bearer ns/sa",
// so tests choose the identity without a Kubernetes API.
type headerAuthenticator struct{}

func (headerAuthenticator) Authenticate(_ context.Context, request *http.Request) (auth.Identity, error) {
	value, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")

	switch {
	case !ok:
		return auth.Identity{}, auth.ErrNoCredential
	case value == "unavailable":
		return auth.Identity{}, auth.ErrUnavailable
	}

	namespace, name, ok := strings.Cut(value, "/")
	if !ok {
		return auth.Identity{}, auth.ErrInvalidCredential
	}

	return auth.Identity{Namespace: namespace, ServiceAccount: name, Pod: "pod-1"}, nil
}

// clock is real time moved forward by an offset the test sets. The fake
// GitHub keeps real time, because it checks App JWTs signed with it.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.offset += d
}

type fixture struct {
	clock   *clock
	fake    *fakegithub.Server
	store   *model.Store
	server  *server.Server
	metrics *server.Metrics
	audit   *bytes.Buffer
	inputs  model.Inputs
}

func keyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})
}

func tokenRule(name string, set map[string]githubgatev1alpha1.Level, repositories ...githubgatev1alpha1.Repository) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:            name,
		InstallationRef: githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: "main"},
		Repositories:    repositories,
		Permissions:     set,
		Token:           &githubgatev1alpha1.TokenMode{Enabled: true},
	}
}

// newFixture is a fake GitHub with an App installed on owner (contents and
// issues write) and repositories foo, bar and baz, and a configuration where
// ci-a's ServiceAccount ci-build may read foo, write bar and write issues on baz.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	fake, err := fakegithub.New(t.TempDir())
	require.NoError(t, err)

	fake.CreateApp(fakegithub.App{ID: appID, Name: "app", PublicKey: &testKey().PublicKey})
	require.NoError(t, fake.CreateInstallation(fakegithub.Installation{
		ID: installationID, AppID: appID, Owner: owner,
		Permissions: map[string]string{"contents": "write", "issues": "write"},
	}))

	for _, name := range []string{"foo", "bar", "baz"} {
		require.NoError(t, fake.CreateRepository(owner, name, false))
	}

	api := httptest.NewServer(fake.Handler())
	t.Cleanup(api.Close)

	id := int64(appID)
	f := &fixture{
		fake:    fake,
		clock:   &clock{},
		store:   &model.Store{},
		metrics: server.NewMetrics(),
		audit:   &bytes.Buffer{},
		inputs: model.Inputs{
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
						Repositories: []githubgatev1alpha1.Repository{"*"},
						Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write", "issues": "write"},
					}},
					AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
				},
			}},
			Policies: []githubgatev1alpha1.GitHubAccessPolicy{{
				ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "policy"},
				Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
					ServiceAccounts: []string{"ci-build"},
					Rules: []githubgatev1alpha1.AccessRule{
						tokenRule("read-foo", map[string]githubgatev1alpha1.Level{"contents": "read"}, "foo"),
						tokenRule("write-bar", map[string]githubgatev1alpha1.Level{"contents": "write"}, "bar"),
						tokenRule("issues-baz", map[string]githubgatev1alpha1.Level{"issues": "write"}, "baz"),
					},
				},
			}},
			Secrets: []corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "app-key", ResourceVersion: "1"},
				Data:       map[string][]byte{"private-key.pem": keyPEM()},
			}},
			Namespaces: []corev1.Namespace{
				{ObjectMeta: metav1.ObjectMeta{Name: adminNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace}},
			},
		},
	}

	f.server = server.New(server.Options{
		Authenticator: headerAuthenticator{},
		Store:         f.store,
		GitHub:        github.NewRegistry(github.Options{BaseURL: api.URL}),
		Metrics:       f.metrics,
		Audit:         slog.New(slog.NewJSONHandler(f.audit, nil)),
		Now:           f.clock.now,
	})
	f.rebuild()

	return f
}

func (f *fixture) rebuild() { f.store.Swap(model.Build(f.inputs)) }

type response struct {
	status int
	header http.Header
	token  server.TokenResponse
	err    server.ErrorResponse
}

func (f *fixture) request(t *testing.T, caller, body string) response {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/token", strings.NewReader(body))
	if caller != "" {
		request.Header.Set("Authorization", "Bearer "+caller)
	}

	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)

	result := response{status: recorder.Code, header: recorder.Header()}
	if recorder.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result.token))
	} else {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result.err))
	}

	return result
}

const caller = tenantNamespace + "/ci-build"

func TestAllowedRequestReturnsTheTokenWithGitHubsRepositoriesAndPermissions(t *testing.T) {
	f := newFixture(t)

	got := f.request(t, caller, `{"repositories": ["foo"]}`)
	require.Equal(t, http.StatusOK, got.status, got.err.Message)

	issued := f.fake.Tokens()
	require.Len(t, issued, 1)
	assert.Equal(t, issued[0].Token, got.token.Token)
	assert.WithinDuration(t, issued[0].ExpiresAt, got.token.ExpiresAt, time.Second)
	assert.Equal(t, []string{"foo"}, got.token.Repositories)
	assert.Equal(t, map[string]permissions.Level{"contents": "read", "metadata": "read"}, got.token.Permissions)
	assert.Equal(t, []string{"foo"}, issued[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "read", "metadata": "read"}, issued[0].Permissions)
}

func TestRepeatedRequestsReuseTheCachedTokenUntilItHasUnderTenMinutesLeft(t *testing.T) {
	f := newFixture(t)

	first := f.request(t, caller, `{"repositories": ["foo"], "permissions": {"contents": "read"}}`)
	require.Equal(t, http.StatusOK, first.status, first.err.Message)

	for range 3 {
		again := f.request(t, caller, `{"repositories": ["foo"], "permissions": {"contents": "read"}}`)
		require.Equal(t, http.StatusOK, again.status)
		assert.Equal(t, first.token.Token, again.token.Token)
	}

	require.Len(t, f.fake.MintLog(), 1)

	// Ten minutes and a few seconds left: still handed out.
	f.clock.advance(first.token.ExpiresAt.Sub(f.clock.now()) - 10*time.Minute - 5*time.Second)

	again := f.request(t, caller, `{"repositories": ["foo"], "permissions": {"contents": "read"}}`)
	require.Equal(t, http.StatusOK, again.status)
	assert.Equal(t, first.token.Token, again.token.Token)
	require.Len(t, f.fake.MintLog(), 1)

	// Under ten minutes left: replaced.
	f.clock.advance(10 * time.Second)

	replaced := f.request(t, caller, `{"repositories": ["foo"], "permissions": {"contents": "read"}}`)
	require.Equal(t, http.StatusOK, replaced.status)
	assert.NotEqual(t, first.token.Token, replaced.token.Token)
	require.Len(t, f.fake.MintLog(), 2)

	gathered := gather(t, f)
	assert.Contains(t, gathered, `githubgate_token_cache_total{result="hit"} 4`)
	assert.Contains(t, gathered, `githubgate_token_cache_total{result="miss"} 2`)
}

func TestConcurrentIdenticalRequestsCauseOneMint(t *testing.T) {
	f := newFixture(t)

	var group sync.WaitGroup

	statuses := make([]int, 20)
	for index := range statuses {
		group.Go(func() {
			statuses[index] = f.request(t, caller, `{"repositories": ["foo"]}`).status
		})
	}

	group.Wait()

	for _, status := range statuses {
		assert.Equal(t, http.StatusOK, status)
	}

	assert.Len(t, f.fake.MintLog(), 1)
}

func TestRemovedGrantIsRefusedEvenWithACachedToken(t *testing.T) {
	f := newFixture(t)

	require.Equal(t, http.StatusOK, f.request(t, caller, `{"repositories": ["foo"]}`).status)

	f.inputs.Policies[0].Spec.Rules = f.inputs.Policies[0].Spec.Rules[1:]
	f.rebuild()

	got := f.request(t, caller, `{"repositories": ["foo"]}`)
	assert.Equal(t, http.StatusForbidden, got.status)
	assert.Equal(t, "denied", got.err.Error)
	assert.Contains(t, got.err.Message, `"foo"`)
	assert.Len(t, f.fake.MintLog(), 1)
}

func TestRefusedRequestsMintNothing(t *testing.T) {
	tests := []struct {
		name     string
		caller   string
		body     string
		status   int
		reason   string
		contains []string
	}{
		{
			name: "ServiceAccount named by no policy", caller: tenantNamespace + "/other", body: `{"repositories": ["foo"]}`,
			status: http.StatusForbidden, reason: "denied", contains: []string{`"ci-a"`, `"other"`},
		},
		{
			name: "repository outside the policy", caller: caller, body: `{"repositories": ["secret"]}`,
			status: http.StatusForbidden, reason: "denied", contains: []string{`"secret"`},
		},
		{
			name: "permission above the policy", caller: caller, body: `{"repositories": ["foo"], "permissions": {"contents": "write"}}`,
			status: http.StatusForbidden, reason: "denied", contains: []string{"contents"},
		},
		{
			name: "repositories that share no permission", caller: caller, body: `{"repositories": ["foo", "baz"]}`,
			status: http.StatusForbidden, reason: "denied", contains: []string{`"foo"`, `"baz"`},
		},
		{
			name: "unknown field", caller: caller, body: `{"repositories": ["foo"], "scope": "all"}`,
			status: http.StatusBadRequest, reason: "invalid_request",
		},
		{
			name: "no credential", body: `{"repositories": ["foo"]}`,
			status: http.StatusUnauthorized, reason: "unauthenticated",
		},
		{
			name: "invalid credential", caller: "garbage", body: `{"repositories": ["foo"]}`,
			status: http.StatusUnauthorized, reason: "unauthenticated",
		},
		{
			name: "authentication unavailable", caller: "unavailable", body: `{"repositories": ["foo"]}`,
			status: http.StatusServiceUnavailable, reason: "unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			got := f.request(t, test.caller, test.body)
			assert.Equal(t, test.status, got.status)
			assert.Equal(t, test.reason, got.err.Error)

			for _, part := range test.contains {
				assert.Contains(t, got.err.Message, part)
			}

			if test.status == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", got.header.Get("WWW-Authenticate"))
			}

			assert.Empty(t, f.fake.MintLog())
		})
	}
}

func TestAppWithABrokenKeySecretFailsClosedUntilTheSecretIsFixed(t *testing.T) {
	tests := []struct {
		name   string
		damage func(secret *corev1.Secret)
	}{
		{name: "missing key", damage: func(secret *corev1.Secret) { secret.Data = map[string][]byte{} }},
		{name: "not an RSA key", damage: func(secret *corev1.Secret) {
			secret.Data = map[string][]byte{"private-key.pem": []byte("not a key")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			good := f.inputs.Secrets[0]
			broken := *good.DeepCopy()
			test.damage(&broken)
			broken.ResourceVersion = "2"
			f.inputs.Secrets = []corev1.Secret{broken}
			f.rebuild()

			got := f.request(t, caller, `{"repositories": ["foo"]}`)
			assert.Equal(t, http.StatusServiceUnavailable, got.status)
			assert.Equal(t, "unavailable", got.err.Error)
			assert.Empty(t, f.fake.MintLog())

			good.ResourceVersion = "3"
			f.inputs.Secrets = []corev1.Secret{good}
			f.rebuild()

			got = f.request(t, caller, `{"repositories": ["foo"]}`)
			assert.Equal(t, http.StatusOK, got.status, got.err.Message)
		})
	}
}

func TestEveryRequestWritesOneAuditLineWithoutTheToken(t *testing.T) {
	f := newFixture(t)

	allowed := f.request(t, caller, `{"repositories": ["foo"]}`)
	require.Equal(t, http.StatusOK, allowed.status)
	f.request(t, caller, `{"repositories": ["secret"]}`)
	f.request(t, "", `{}`)

	assert.NotContains(t, f.audit.String(), allowed.token.Token)

	lines := strings.Split(strings.TrimSpace(f.audit.String()), "\n")
	require.Len(t, lines, 3)

	entries := make([]map[string]any, len(lines))
	for index, line := range lines {
		require.NoError(t, json.Unmarshal([]byte(line), &entries[index]))
	}

	assert.Equal(t, "ci-a", entries[0]["namespace"])
	assert.Equal(t, "ci-build", entries[0]["service_account"])
	assert.Equal(t, "pod-1", entries[0]["pod"])
	assert.Equal(t, "githubgate/main", entries[0]["installation"])
	assert.Equal(t, "token", entries[0]["mode"])
	assert.Equal(t, []any{"foo"}, entries[0]["repositories"])
	assert.Equal(t, "contents:read", entries[0]["permissions"])
	assert.Equal(t, "allowed", entries[0]["decision"])
	assert.Equal(t, "minted", entries[0]["github_status"])
	assert.Contains(t, entries[0], "duration")
	assert.NotEmpty(t, entries[0]["request_id"])

	assert.Equal(t, "refused", entries[1]["decision"])
	assert.Equal(t, "denied", entries[1]["reason"])
	assert.Equal(t, []any{"secret"}, entries[1]["repositories"])

	assert.Equal(t, "refused", entries[2]["decision"])
	assert.Equal(t, "unauthenticated", entries[2]["reason"])
}

func TestMetricsCountRequestsByDecision(t *testing.T) {
	f := newFixture(t)

	f.request(t, caller, `{"repositories": ["foo"]}`)
	f.request(t, caller, `{"repositories": ["secret"]}`)
	f.request(t, caller, `{"repositories": ["secret"]}`)

	gathered := gather(t, f)
	assert.Contains(t, gathered, `githubgate_requests_total{code_class="2xx",decision="allowed",mode="token"} 1`)
	assert.Contains(t, gathered, `githubgate_requests_total{code_class="4xx",decision="refused",mode="token"} 2`)
	assert.Contains(t, gathered, `githubgate_tokenreview_total{result="authenticated"} 3`)
	assert.Contains(t, gathered, `githubgate_github_request_duration_seconds_count{operation="mint"} 1`)
}

func TestRequestsBeforeTheFirstSnapshotGetNotReady(t *testing.T) {
	f := newFixture(t)
	f.store = &model.Store{}
	f.server = server.New(server.Options{Authenticator: headerAuthenticator{}, Store: f.store})

	got := f.request(t, caller, `{"repositories": ["foo"]}`)
	assert.Equal(t, http.StatusServiceUnavailable, got.status)
	assert.Equal(t, "not_ready", got.err.Error)
	assert.Equal(t, http.StatusServiceUnavailable, readyz(t, f.server))

	f.rebuild()
	assert.Equal(t, http.StatusOK, readyz(t, f.server))
}

// gather returns the text exposition of /metrics.
func gather(t *testing.T, f *fixture) string {
	t.Helper()

	registry := prometheus.NewRegistry()
	registry.MustRegister(f.metrics.Collectors()...)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	f.server.HealthHandler(registry).ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)

	return string(body)
}

func readyz(t *testing.T, s *server.Server) int {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	s.HealthHandler(prometheus.NewRegistry()).ServeHTTP(recorder, request)

	return recorder.Code
}

// paddedBody is a valid request padded with spaces to exactly size bytes.
func paddedBody(size int) string {
	const prefix, suffix = `{"repositories": ["foo"]`, `}`

	return prefix + strings.Repeat(" ", size-len(prefix)-len(suffix)) + suffix
}

func TestBodyLimitIs64KiB(t *testing.T) {
	f := newFixture(t)

	got := f.request(t, caller, paddedBody(64<<10))
	assert.Equal(t, http.StatusOK, got.status, got.err.Message)

	got = f.request(t, caller, paddedBody(64<<10+1))
	assert.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, "invalid_request", got.err.Error)
	assert.Contains(t, got.err.Message, "larger than")
}

func TestRulesThePolicyCannotUseGrantNothing(t *testing.T) {
	tests := []struct {
		name   string
		caller string
		modify func(inputs *model.Inputs)
		body   string
		status int
	}{
		{
			name:   "a rule outside the installation's ceiling grants nothing",
			caller: caller,
			modify: func(inputs *model.Inputs) {
				inputs.Installations[0].Spec.Ceiling = []githubgatev1alpha1.PermissionSet{{
					Repositories: []githubgatev1alpha1.Repository{"foo"},
					Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write"},
				}}
			},
			body:   `{"repositories": ["bar"]}`,
			status: http.StatusForbidden,
		},
		{
			name:   "a rule inside the ceiling still works when another rule is outside it",
			caller: caller,
			modify: func(inputs *model.Inputs) {
				inputs.Installations[0].Spec.Ceiling = []githubgatev1alpha1.PermissionSet{{
					Repositories: []githubgatev1alpha1.Repository{"foo"},
					Permissions:  map[string]githubgatev1alpha1.Level{"contents": "write"},
				}}
			},
			body:   `{"repositories": ["foo"]}`,
			status: http.StatusOK,
		},
		{
			name:   "an installation that doesn't admit the policy's namespace grants nothing",
			caller: caller,
			modify: func(inputs *model.Inputs) {
				inputs.Installations[0].Spec.AllowedNamespaces = githubgatev1alpha1.AllowedNamespaces{Names: []string{"ci-other"}}
			},
			body:   `{"repositories": ["foo"]}`,
			status: http.StatusForbidden,
		},
		{
			name:   "a ServiceAccount of another namespace gets nothing from the policy",
			caller: "ci-b/ci-build",
			body:   `{"repositories": ["foo"]}`,
			status: http.StatusForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			if test.modify != nil {
				test.modify(&f.inputs)
				f.rebuild()
			}

			got := f.request(t, test.caller, test.body)
			assert.Equal(t, test.status, got.status, got.err.Message)

			if test.status != http.StatusOK {
				assert.Empty(t, f.fake.MintLog())
			}
		})
	}
}

func TestTokenModeUsesOnlyRulesThatEnableIt(t *testing.T) {
	proxyOnly := func(inputs *model.Inputs) {
		rule := &inputs.Policies[0].Spec.Rules[0]
		rule.Token = nil
		rule.Proxy = &githubgatev1alpha1.ProxyMode{Enabled: true}
	}

	tests := []struct {
		name   string
		modify func(inputs *model.Inputs)
		status int
	}{
		{name: "a rule with token mode gives a token", status: http.StatusOK},
		{name: "a rule with only proxy mode gives no token", modify: proxyOnly, status: http.StatusForbidden},
		{
			name: "a rule with both modes gives a token",
			modify: func(inputs *model.Inputs) {
				inputs.Policies[0].Spec.Rules[0].Proxy = &githubgatev1alpha1.ProxyMode{Enabled: true}
			},
			status: http.StatusOK,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			if test.modify != nil {
				test.modify(&f.inputs)
				f.rebuild()
			}

			got := f.request(t, caller, `{"repositories": ["foo"]}`)
			assert.Equal(t, test.status, got.status, got.err.Message)

			if test.status != http.StatusOK {
				assert.Equal(t, "denied", got.err.Error)
				assert.Empty(t, f.fake.MintLog())
			}
		})
	}
}
