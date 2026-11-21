package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const (
	createSigningKey = "users/create-ssh-signing-key-for-authenticated-user"
	deleteSigningKey = "users/delete-ssh-signing-key-for-authenticated-user"
	listEmails       = "users/list-emails-for-authenticated-user"
	getUser          = "users/get-authenticated"
)

var (
	ownerAuthorization = &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "owner-user"},
		Login:          "octocat", Resolved: true, Reason: model.ReasonReady,
	}
	otherAuthorization = &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "other-user"},
		Login:          "hubot", Resolved: true, Reason: model.ReasonReady,
	}
)

// userRule is an accepted user rule of authorization listing operations.
func userRule(name string, authorization *model.UserAuthorization, operations ...string) *model.Rule {
	return &model.Rule{
		Policy: types.NamespacedName{Namespace: "ci-a", Name: "policy"}, Name: name,
		User: &model.UserRule{
			Authorization: authorization.NamespacedName, View: authorization,
			Operations: operations, Accepted: true, Reason: model.ReasonAccepted,
		},
	}
}

func notAccepted(rule *model.Rule, reason string) *model.Rule {
	rule.User.Accepted, rule.User.Reason = false, reason

	return rule
}

func TestDecideByUserRules(t *testing.T) {
	notResolved := &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "pending-user"},
		Login:          "octocat", Reason: model.ReasonRefNotReady,
	}
	// An installation rule that would allow every request below on its own.
	installationRule := proxyRule("installation", app1Installation, []string{"foo"}, write("contents"),
		allowOperations(createSigningKey, "repos/get"))

	tests := []struct {
		name                  string
		rules                 []*model.Rule
		request               proxy.Decided
		expectedStatus        int
		expectedCode          string
		expectedMessage       string
		expectedAccess        proxy.Access
		expectedAuthorization *model.UserAuthorization
	}{
		{
			name:                  "an operation an accepted user rule lists is forwarded as that user",
			rules:                 []*model.Rule{installationRule, userRule("keys", ownerAuthorization, createSigningKey)},
			request:               rest(createSigningKey, "", ""),
			expectedAccess:        proxy.AccessUser,
			expectedAuthorization: ownerAuthorization,
		},
		{
			name: "two user rules of one authorization forward as that user",
			rules: []*model.Rule{
				userRule("create", ownerAuthorization, createSigningKey),
				userRule("both", ownerAuthorization, createSigningKey, deleteSigningKey),
			},
			request:               rest(createSigningKey, "", ""),
			expectedAccess:        proxy.AccessUser,
			expectedAuthorization: ownerAuthorization,
		},
		{
			name:           "an operation no user rule lists is decided by the installation rules",
			rules:          []*model.Rule{installationRule, userRule("keys", ownerAuthorization, createSigningKey)},
			request:        rest("repos/get", "acme", "foo"),
			expectedAccess: proxy.AccessRules,
		},
		{
			name:            "a user operation no user rule lists is decided by the installation rules",
			rules:           []*model.Rule{userRule("keys", ownerAuthorization, createSigningKey)},
			request:         rest(listEmails, "", ""),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "no proxy rule allows it",
		},
		{
			name:  "a listed operation on the uploads host is decided by the installation rules",
			rules: []*model.Rule{userRule("keys", ownerAuthorization, createSigningKey)},
			request: proxy.Decided{Request: proxy.Request{
				Host: proxy.HostUploads, Kind: proxy.KindREST, Operation: createSigningKey,
			}},
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: "no proxy rule allows it",
		},
		{
			name:           "a GraphQL request is decided by the installation rules",
			rules:          []*model.Rule{installationRule, userRule("keys", ownerAuthorization, createSigningKey)},
			request:        mutation(),
			expectedAccess: proxy.AccessRules,
		},
		{
			name:           "a git request is decided by the installation rules",
			rules:          []*model.Rule{installationRule, userRule("keys", ownerAuthorization, createSigningKey)},
			request:        git(proxy.KindGitUpload, "foo", permissions.LevelRead),
			expectedAccess: proxy.AccessRules,
		},
		{
			name:            "a github.com web path is decided by the installation rules",
			rules:           []*model.Rule{userRule("keys", ownerAuthorization, createSigningKey)},
			request:         proxy.Decided{Request: proxy.Request{Host: proxy.HostGit, Kind: proxy.KindWeb}},
			expectedStatus:  http.StatusNotFound,
			expectedCode:    "not_found",
			expectedMessage: "only git smart HTTP is served on github.com",
		},
		{
			name: "two authorizations covering one operation are refused",
			rules: []*model.Rule{
				installationRule,
				userRule("owner", ownerAuthorization, createSigningKey),
				userRule("other", otherAuthorization, createSigningKey),
			},
			request:         rest(createSigningKey, "", ""),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `operation "` + createSigningKey + `": covered by more than one user authorization`,
		},
		{
			name: "an operation listed by a user rule that isn't accepted is refused, not decided by the installation rules",
			rules: []*model.Rule{
				installationRule,
				notAccepted(userRule("keys", ownerAuthorization, createSigningKey), model.ReasonOutsideCeiling),
			},
			request:         rest(createSigningKey, "", ""),
			expectedStatus:  http.StatusForbidden,
			expectedCode:    "denied",
			expectedMessage: `operation "` + createSigningKey + `": user rule "keys" is not accepted: OutsideCeiling`,
		},
		{
			name:            "an authorization that is not resolved answers 503 naming it",
			rules:           []*model.Rule{installationRule, userRule("keys", notResolved, createSigningKey)},
			request:         rest(createSigningKey, "", ""),
			expectedStatus:  http.StatusServiceUnavailable,
			expectedCode:    "unavailable",
			expectedMessage: "user authorization githubgate/pending-user is not ready: RefNotReady",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := proxy.Decide(test.rules, test.request)

			assert.Equal(t, test.expectedStatus, decision.Status, decision.Message)
			assert.Equal(t, test.expectedCode, decision.Code)
			assert.Equal(t, test.expectedMessage, decision.Message)

			if test.expectedStatus != 0 {
				assert.Nil(t, decision.Installation, "a refusal gets no installation token")
				assert.Nil(t, decision.UserAuthorization, "a refusal gets no user token")

				return
			}

			assert.Equal(t, test.expectedAccess, decision.Access)
			assert.Same(t, test.expectedAuthorization, decision.UserAuthorization)

			if test.expectedAccess == proxy.AccessUser {
				assert.Nil(t, decision.Installation, "a user request gets no installation token")
			}
		})
	}
}

// fakeUserTokens hands out one access token per authorization, or a reason
// when none is stored.
type fakeUserTokens struct {
	mu     sync.Mutex
	tokens map[types.NamespacedName]string
	reason string
	// refused lists the authorizations whose token GitHub refused.
	refused []types.NamespacedName
}

func (f *fakeUserTokens) UserTokenRefused(_ context.Context, authorization *model.UserAuthorization) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.refused = append(f.refused, authorization.NamespacedName)
}

func (f *fakeUserTokens) UserToken(_ context.Context, authorization *model.UserAuthorization) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if token := f.tokens[authorization.NamespacedName]; token != "" {
		return token, "", nil
	}

	return "", f.reason, nil
}

// userProxy is a proxy whose upstream is the fake GitHub, with octocat's
// user token authorized through the device flow.
type userProxy struct {
	handler     http.Handler
	github      *fakegithub.Server
	tokens      *fakeTokens
	userTokens  *fakeUserTokens
	audit       *bytes.Buffer
	accessToken string
}

func newUserProxy(t *testing.T, rules ...*model.Rule) *userProxy {
	t.Helper()

	github, err := fakegithub.New(t.TempDir())
	require.NoError(t, err)

	github.CreateOAuthApp(fakegithub.OAuthApp{ClientID: "Iv1.proxy", DeviceFlow: true})
	github.SetDeviceInterval(0)

	server := httptest.NewServer(github.Handler())
	t.Cleanup(server.Close)

	config := &oauth2.Config{ClientID: "Iv1.proxy", Endpoint: oauth2.Endpoint{
		DeviceAuthURL: server.URL + "/login/device/code",
		TokenURL:      server.URL + "/login/oauth/access_token",
		AuthStyle:     oauth2.AuthStyleInParams,
	}}

	device, err := config.DeviceAuth(t.Context())
	require.NoError(t, err)
	require.NoError(t, github.ApproveDeviceCode(device.UserCode, "octocat"))

	// DeviceAccessToken waits at least five seconds before its first poll;
	// the code is approved already, so one exchange gets the token.
	token, err := config.Exchange(t.Context(), "",
		oauth2.SetAuthURLParam("grant_type", "urn:ietf:params:oauth:grant-type:device_code"),
		oauth2.SetAuthURLParam("device_code", device.DeviceCode))
	require.NoError(t, err)

	base, err := url.Parse(server.URL)
	require.NoError(t, err)

	tokens := &fakeTokens{}
	userTokens := &fakeUserTokens{tokens: map[types.NamespacedName]string{ownerAuthorization.NamespacedName: token.AccessToken}}
	audit := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{writer: audit}, nil))

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:      func(string, string) []*model.Rule { return rules },
		Tokens:     tokens,
		UserTokens: userTokens,
		Upstreams:  proxy.Upstreams{API: base, Uploads: base, Git: base},
		Audit:      logger,
	})
	require.NoError(t, err)

	return &userProxy{
		handler:     proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder, Audit: logger}),
		github:      github,
		tokens:      tokens,
		userTokens:  userTokens,
		audit:       audit,
		accessToken: token.AccessToken,
	}
}

func (p *userProxy) auditLines(t *testing.T) []map[string]any {
	t.Helper()

	lines := []map[string]any{}

	for line := range strings.SplitSeq(strings.TrimSpace(p.audit.String()), "\n") {
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &fields))
		lines = append(lines, fields)
	}

	return lines
}

// assertNoUserTokenLeaked fails when any user or refresh token the fake
// issued appears in what the caller got or in the audit log.
func (p *userProxy) assertNoUserTokenLeaked(t *testing.T, responses ...*httptest.ResponseRecorder) {
	t.Helper()

	issued := p.github.UserTokens()
	require.NotEmpty(t, issued, "the fake issued user tokens")

	corpus := p.audit.String()
	for _, response := range responses {
		corpus += response.Body.String()

		for name, values := range response.Header() {
			corpus += name + ": " + strings.Join(values, ",") + "\n"
		}
	}

	for _, token := range issued {
		require.NotEmpty(t, token.Token)
		assert.NotContains(t, corpus, token.Token, "a %s token leaked", token.Kind)
	}

	for _, code := range p.github.DeviceCodes() {
		require.NotEmpty(t, code)
		assert.NotContains(t, corpus, code, "a device code leaked")
	}
}

func TestProxyForwardsUserRuleRequestsWithTheUserAccessToken(t *testing.T) {
	user := newUserProxy(t, userRule("create", ownerAuthorization, createSigningKey), userRule("delete", ownerAuthorization, deleteSigningKey))

	created := authenticated(http.MethodPost, proxy.HostAPI, "/user/ssh_signing_keys")
	response := serve(user.handler, withBody(created, `{"title":"workspace","key":"ssh-ed25519 AAAAworkspace"}`))

	// The fake answers user endpoints only to a user access token, so 201
	// means GitHub got octocat's token and not the caller's credential.
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())

	keys := user.github.SSHSigningKeys("octocat")
	require.Len(t, keys, 1)
	assert.Equal(t, "workspace", keys[0].Title)

	deleted := serve(user.handler, authenticated(http.MethodDelete, proxy.HostAPI, "/user/ssh_signing_keys/"+strconv.FormatInt(keys[0].ID, 10)))
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	assert.Empty(t, user.github.SSHSigningKeys("octocat"))

	assert.Empty(t, user.tokens.decisions, "no installation token is asked for")

	lines := user.auditLines(t)
	require.Len(t, lines, 2)

	for index, operation := range []string{createSigningKey, deleteSigningKey} {
		assert.Equal(t, operation, lines[index]["operation"])
		assert.Equal(t, "user", lines[index]["access"])
		assert.Equal(t, "githubgate/owner-user", lines[index]["user_authorization"])
		assert.Equal(t, "octocat", lines[index]["login"])
		assert.Equal(t, "allowed", lines[index]["decision"])
		assert.Empty(t, lines[index]["installation"])
	}

	user.assertNoUserTokenLeaked(t, response, deleted)
}

func TestProxyNeverSendsTheUserTokenForOperationsNoUserRuleLists(t *testing.T) {
	user := newUserProxy(t,
		userRule("create", ownerAuthorization, createSigningKey),
		proxyRule("installation", app1Installation, []string{"foo"}, read("contents"), allowOperations(getUser)),
	)

	response := serve(user.handler, authenticated(http.MethodGet, proxy.HostAPI, "/user"))

	// The installation rule allows it, so it goes with the test's
	// installation token, which the fake GitHub doesn't know. Sent with the
	// user token, it would have succeeded.
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "github_unavailable")
	require.Len(t, user.tokens.decisions, 1, "the installation rules decide it")
	assert.Equal(t, app1Installation, user.tokens.decisions[0].Installation)

	lines := user.auditLines(t)
	require.Len(t, lines, 1)
	assert.Equal(t, "rules", lines[0]["access"])
	assert.Empty(t, lines[0]["user_authorization"])

	user.assertNoUserTokenLeaked(t, response)
}

func TestProxyRefusesUserRuleRequestsWithoutAUsableToken(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(user *userProxy)
		reason  string
		// refused is how many times the leader is asked to refresh.
		refused int
	}{
		{
			name: "no stored token answers 503 naming the authorization",
			prepare: func(user *userProxy) {
				user.userTokens.tokens = nil
				user.userTokens.reason = "AuthorizationRequired"
			},
			reason: "AuthorizationRequired",
		},
		{
			name: "a stored token GitHub refuses answers 503 naming the authorization",
			prepare: func(user *userProxy) {
				user.userTokens.tokens = map[types.NamespacedName]string{ownerAuthorization.NamespacedName: "ghu_revokedEarly"}
			},
			reason:  "GitHub refused its access token",
			refused: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user := newUserProxy(t,
				userRule("create", ownerAuthorization, createSigningKey),
				proxyRule("installation", app1Installation, []string{"foo"}, write("contents"), allowOperations(createSigningKey)),
			)
			test.prepare(user)

			request := authenticated(http.MethodPost, proxy.HostAPI, "/user/ssh_signing_keys")
			response := serve(user.handler, withBody(request, `{"title":"workspace","key":"ssh-ed25519 AAAAworkspace"}`))

			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

			var body map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, "unavailable", body["error"])
			assert.Equal(t, "githubgate: user authorization githubgate/owner-user is not ready: "+test.reason, body["message"])

			assert.Empty(t, user.tokens.decisions, "it never falls back to an installation token")
			assert.Empty(t, user.github.SSHSigningKeys("octocat"))

			user.userTokens.mu.Lock()
			assert.Len(t, user.userTokens.refused, test.refused)
			user.userTokens.mu.Unlock()

			lines := user.auditLines(t)
			require.Len(t, lines, 1)
			assert.Equal(t, "refused", lines[0]["decision"])
			assert.Equal(t, "unavailable", lines[0]["reason"])

			user.assertNoUserTokenLeaked(t, response)
		})
	}
}
