package fakegithub_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const testClientID = "Iv1.test"

type userFixture struct {
	*fixture
	config *oauth2.Config
}

func newUserFixture(t *testing.T) *userFixture {
	t.Helper()

	base := newFixture(t)
	base.server.CreateOAuthApp(fakegithub.OAuthApp{ClientID: testClientID, DeviceFlow: true})
	base.server.SetDeviceInterval(0)

	return &userFixture{fixture: base, config: &oauth2.Config{
		ClientID: testClientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: base.api.URL + "/login/device/code",
			TokenURL:      base.api.URL + "/login/oauth/access_token",
		},
	}}
}

// oauthPost posts a form and returns the status and the decoded JSON answer.
func (f *userFixture) oauthPost(t *testing.T, path string, form url.Values, accept string) (int, string, map[string]any) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.api.URL+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if accept != "" {
		request.Header.Set("Accept", accept)
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	out := map[string]any{}

	if accept == "application/json" {
		require.NoError(t, json.Unmarshal(raw, &out))
	} else {
		values, err := url.ParseQuery(string(raw))
		require.NoError(t, err)

		for key := range values {
			out[key] = values.Get(key)
		}
	}

	return response.StatusCode, string(raw), out
}

func (f *userFixture) startDevice(t *testing.T) (deviceCode, userCode string) {
	t.Helper()

	_, _, out := f.oauthPost(t, "/login/device/code", url.Values{"client_id": {testClientID}}, "application/json")
	require.NotEmpty(t, out["device_code"])

	return out["device_code"].(string), out["user_code"].(string)
}

func (f *userFixture) poll(t *testing.T, deviceCode string) (string, map[string]any) {
	t.Helper()

	_, _, out := f.oauthPost(t, "/login/oauth/access_token", url.Values{
		"client_id": {testClientID}, "device_code": {deviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	}, "application/json")

	code, _ := out["error"].(string)

	return code, out
}

func (f *userFixture) refresh(t *testing.T, refreshToken string) (string, map[string]any) {
	t.Helper()

	_, _, out := f.oauthPost(t, "/login/oauth/access_token", url.Values{
		"client_id": {testClientID}, "refresh_token": {refreshToken}, "grant_type": {"refresh_token"},
	}, "application/json")

	code, _ := out["error"].(string)

	return code, out
}

// authorize runs the whole device flow as login and returns the token pair.
func (f *userFixture) authorize(t *testing.T, login string) (access, refresh string) {
	t.Helper()

	device, user := f.startDevice(t)
	require.NoError(t, f.server.ApproveDeviceCode(user, login))

	code, out := f.poll(t, device)
	require.Empty(t, code)

	return out["access_token"].(string), out["refresh_token"].(string)
}

func (f *userFixture) call(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, f.api.URL+path, strings.NewReader(body))
	require.NoError(t, err)

	request.Header.Set("Authorization", "Bearer "+token)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, string(raw)
}

func TestDeviceFlowAnswersLikeGitHub(t *testing.T) {
	t.Run("pending until approved, then tokens through the oauth2 library", func(t *testing.T) {
		f := newUserFixture(t)

		auth, err := f.config.DeviceAuth(t.Context())
		require.NoError(t, err)
		assert.Regexp(t, `^[A-Z]{4}-[A-Z]{4}$`, auth.UserCode)
		assert.Equal(t, f.api.URL+"/login/device", auth.VerificationURI)

		code, _ := f.poll(t, auth.DeviceCode)
		assert.Equal(t, "authorization_pending", code)

		require.NoError(t, f.server.ApproveDeviceCode(auth.UserCode, "octocat"))

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		token, err := f.config.DeviceAccessToken(ctx, auth)
		require.NoError(t, err)
		assert.NotEmpty(t, token.AccessToken)
		assert.NotEmpty(t, token.RefreshToken)
		assert.WithinDuration(t, time.Now().Add(8*time.Hour), token.Expiry, time.Minute)
		assert.InDelta(t, (6 * 30 * 24 * time.Hour).Seconds(), token.Extra("refresh_token_expires_in"), 1)

		status, body := f.call(t, http.MethodGet, "/user", token.AccessToken, "")
		require.Equal(t, http.StatusOK, status)
		assert.JSONEq(t, `{"login":"octocat","type":"User"}`, body)
	})

	t.Run("slow_down when polled faster than the interval", func(t *testing.T) {
		f := newUserFixture(t)
		f.server.SetDeviceInterval(5 * time.Second)

		device, _ := f.startDevice(t)

		code, _ := f.poll(t, device)
		assert.Equal(t, "authorization_pending", code)

		code, out := f.poll(t, device)
		assert.Equal(t, "slow_down", code)
		assert.InDelta(t, 10, out["interval"], 0)

		f.server.AdvanceClock(11 * time.Second)

		code, _ = f.poll(t, device)
		assert.Equal(t, "authorization_pending", code)
	})

	t.Run("expired_token after the code lifetime", func(t *testing.T) {
		f := newUserFixture(t)
		device, user := f.startDevice(t)
		require.NoError(t, f.server.ApproveDeviceCode(user, "octocat"))

		f.server.AdvanceClock(16 * time.Minute)

		code, _ := f.poll(t, device)
		assert.Equal(t, "expired_token", code)
	})

	t.Run("access_denied after the person refuses", func(t *testing.T) {
		f := newUserFixture(t)
		device, user := f.startDevice(t)
		require.NoError(t, f.server.DenyDeviceCode(user))

		code, _ := f.poll(t, device)
		assert.Equal(t, "access_denied", code)
	})

	t.Run("device_flow_disabled when the App's switch is off", func(t *testing.T) {
		f := newUserFixture(t)
		require.NoError(t, f.server.SetDeviceFlow(testClientID, false))

		_, _, out := f.oauthPost(t, "/login/device/code", url.Values{"client_id": {testClientID}}, "application/json")
		assert.Equal(t, "device_flow_disabled", out["error"])
	})

	t.Run("a device code is used once", func(t *testing.T) {
		f := newUserFixture(t)
		device, user := f.startDevice(t)
		require.NoError(t, f.server.ApproveDeviceCode(user, "octocat"))

		code, _ := f.poll(t, device)
		require.Empty(t, code)

		code, _ = f.poll(t, device)
		assert.Equal(t, "bad_verification_code", code)
	})

	t.Run("errors come with status 200 and form encoding unless JSON is asked for", func(t *testing.T) {
		f := newUserFixture(t)
		device, _ := f.startDevice(t)

		status, raw, out := f.oauthPost(t, "/login/oauth/access_token", url.Values{
			"client_id": {testClientID}, "device_code": {device}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}, "")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "authorization_pending", out["error"])
		assert.NotContains(t, raw, "{")
	})

	t.Run("an unknown client ID is refused", func(t *testing.T) {
		f := newUserFixture(t)

		_, _, out := f.oauthPost(t, "/login/device/code", url.Values{"client_id": {"other"}}, "application/json")
		assert.Equal(t, "incorrect_client_credentials", out["error"])
	})
}

func TestRefreshTokensWorkOnce(t *testing.T) {
	t.Run("refresh returns a new pair and the used refresh token stops working", func(t *testing.T) {
		f := newUserFixture(t)
		access, refresh := f.authorize(t, "octocat")

		code, out := f.refresh(t, refresh)
		require.Empty(t, code)
		assert.NotEqual(t, access, out["access_token"])
		assert.NotEqual(t, refresh, out["refresh_token"])

		code, _ = f.refresh(t, refresh)
		assert.Equal(t, "bad_refresh_token", code)

		code, _ = f.refresh(t, out["refresh_token"].(string))
		assert.Empty(t, code)
	})

	t.Run("refresh through the oauth2 library", func(t *testing.T) {
		f := newUserFixture(t)
		_, refresh := f.authorize(t, "octocat")

		token, err := f.config.TokenSource(t.Context(), &oauth2.Token{RefreshToken: refresh, Expiry: time.Now().Add(-time.Hour)}).Token()
		require.NoError(t, err)
		assert.NotEqual(t, refresh, token.RefreshToken)
		assert.NotEmpty(t, token.AccessToken)
	})

	t.Run("a reuse is counted and calls the hook", func(t *testing.T) {
		f := newUserFixture(t)

		var (
			mu     sync.Mutex
			hooked []string
		)

		f.server.OnRefreshReuse(func(token string) {
			mu.Lock()
			defer mu.Unlock()

			hooked = append(hooked, token)
		})

		_, refresh := f.authorize(t, "octocat")

		f.refresh(t, refresh)
		assert.Empty(t, f.server.RefreshReuses())

		f.refresh(t, refresh)
		assert.Equal(t, []string{refresh}, f.server.RefreshReuses())

		mu.Lock()
		defer mu.Unlock()

		assert.Equal(t, []string{refresh}, hooked)
	})

	t.Run("token lifetimes are set by tests", func(t *testing.T) {
		f := newUserFixture(t)
		f.server.SetUserTokenLifetimes(time.Minute, time.Hour)

		access, refresh := f.authorize(t, "octocat")

		status, _ := f.call(t, http.MethodGet, "/user", access, "")
		assert.Equal(t, http.StatusOK, status)

		f.server.AdvanceClock(2 * time.Minute)

		status, _ = f.call(t, http.MethodGet, "/user", access, "")
		assert.Equal(t, http.StatusUnauthorized, status)

		f.server.AdvanceClock(2 * time.Hour)

		code, _ := f.refresh(t, refresh)
		assert.Equal(t, "bad_refresh_token", code)
	})

	t.Run("a refresh token of another client ID is refused", func(t *testing.T) {
		f := newUserFixture(t)
		f.server.CreateOAuthApp(fakegithub.OAuthApp{ClientID: "other", DeviceFlow: true})
		_, refresh := f.authorize(t, "octocat")

		_, _, out := f.oauthPost(t, "/login/oauth/access_token", url.Values{
			"client_id": {"other"}, "refresh_token": {refresh}, "grant_type": {"refresh_token"},
		}, "application/json")
		assert.Equal(t, "bad_refresh_token", out["error"])
	})
}

func TestUserEndpointsAcceptOnlyUserTokens(t *testing.T) {
	f := newUserFixture(t)
	access, _ := f.authorize(t, "octocat")
	installation := f.token(t, map[string]any{})

	paths := []struct{ method, path, body string }{
		{http.MethodGet, "/user", ""},
		{http.MethodGet, "/user/ssh_signing_keys", ""},
		{http.MethodPost, "/user/ssh_signing_keys", `{"title":"t","key":"ssh-ed25519 AAAA"}`},
		{http.MethodDelete, "/user/ssh_signing_keys/1", ""},
	}

	for _, endpoint := range paths {
		t.Run("installation token on "+endpoint.method+" "+endpoint.path, func(t *testing.T) {
			status, body := f.call(t, endpoint.method, endpoint.path, installation, endpoint.body)
			assert.Equal(t, http.StatusForbidden, status)
			assert.Contains(t, body, "Resource not accessible by integration")
		})

		t.Run("unknown token on "+endpoint.method+" "+endpoint.path, func(t *testing.T) {
			status, _ := f.call(t, endpoint.method, endpoint.path, "ghu_unknown", endpoint.body)
			assert.Equal(t, http.StatusUnauthorized, status)
		})
	}

	t.Run("signing keys are added, listed and deleted for the token's user only", func(t *testing.T) {
		status, body := f.call(t, http.MethodPost, "/user/ssh_signing_keys", access, `{"title":"ws","key":"ssh-ed25519 AAAA"}`)
		require.Equal(t, http.StatusCreated, status)

		var created fakegithub.SSHSigningKey
		require.NoError(t, json.Unmarshal([]byte(body), &created))
		assert.Equal(t, "ws", created.Title)

		status, _ = f.call(t, http.MethodPost, "/user/ssh_signing_keys", access, `{"title":"again","key":"ssh-ed25519 AAAA"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, status)

		status, _ = f.call(t, http.MethodPost, "/user/ssh_signing_keys", access, `{"title":"empty"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, status)

		status, body = f.call(t, http.MethodGet, "/user/ssh_signing_keys", access, "")
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, "ssh-ed25519 AAAA")

		other, _ := f.authorize(t, "someone-else")
		status, body = f.call(t, http.MethodGet, "/user/ssh_signing_keys", other, "")
		require.Equal(t, http.StatusOK, status)
		assert.JSONEq(t, `[]`, body)

		status, _ = f.call(t, http.MethodDelete, "/user/ssh_signing_keys/"+strconv.FormatInt(created.ID, 10), other, "")
		assert.Equal(t, http.StatusNotFound, status)

		status, _ = f.call(t, http.MethodDelete, "/user/ssh_signing_keys/"+strconv.FormatInt(created.ID, 10), access, "")
		assert.Equal(t, http.StatusNoContent, status)
		assert.Empty(t, f.server.SSHSigningKeys("octocat"))
	})
}

func TestUserTokensAreRecordedForLeakSearch(t *testing.T) {
	f := newUserFixture(t)
	access, refresh := f.authorize(t, "octocat")

	_, out := f.refresh(t, refresh)

	recorded := map[string]fakegithub.UserTokenKind{}
	for _, issued := range f.server.UserTokens() {
		recorded[issued.Token] = issued.Kind
		assert.Equal(t, "octocat", issued.Login)
	}

	assert.Equal(t, map[string]fakegithub.UserTokenKind{
		access:                        fakegithub.AccessToken,
		refresh:                       fakegithub.RefreshToken,
		out["access_token"].(string):  fakegithub.AccessToken,
		out["refresh_token"].(string): fakegithub.RefreshToken,
	}, recorded)
}

func TestControlAPIDrivesTheDeviceFlow(t *testing.T) {
	f := newUserFixture(t)
	control := httptest.NewServer(f.server.ControlHandler())
	t.Cleanup(control.Close)

	post := func(path, body string) int {
		response, err := http.Post(control.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // test helper
		require.NoError(t, err)

		defer func() { _ = response.Body.Close() }()

		return response.StatusCode
	}

	require.Equal(t, http.StatusCreated, post("/oauth-apps", `{"client_id":"Iv1.ctl","device_flow":true}`))
	require.Equal(t, http.StatusNoContent, post("/device-interval", `{"seconds":0}`))

	_, _, out := f.oauthPost(t, "/login/device/code", url.Values{"client_id": {"Iv1.ctl"}}, "application/json")
	require.Equal(t, http.StatusNoContent, post("/device-codes/approve", `{"user_code":"`+out["user_code"].(string)+`","login":"octocat"}`))

	_, _, token := f.oauthPost(t, "/login/oauth/access_token", url.Values{
		"client_id": {"Iv1.ctl"}, "device_code": {out["device_code"].(string)}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	}, "application/json")
	assert.NotEmpty(t, token["access_token"])

	require.Equal(t, http.StatusNoContent, post("/oauth-apps/Iv1.ctl/device-flow", `{"enabled":false}`))

	_, _, out = f.oauthPost(t, "/login/device/code", url.Values{"client_id": {"Iv1.ctl"}}, "application/json")
	assert.Equal(t, "device_flow_disabled", out["error"])

	response, err := http.Get(control.URL + "/user-tokens") //nolint:noctx // test helper
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	var issued []fakegithub.IssuedUserToken

	require.NoError(t, json.NewDecoder(response.Body).Decode(&issued))
	assert.Len(t, issued, 2)
}
