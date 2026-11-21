package fakegithub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub's lifetimes for the OAuth device flow and expiring user tokens:
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens
// https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow
const (
	defaultUserAccessLifetime  = 8 * time.Hour
	defaultUserRefreshLifetime = 6 * 30 * 24 * time.Hour
	defaultDeviceCodeLifetime  = 15 * time.Minute
	defaultDeviceInterval      = 5 * time.Second
	slowDownIncrement          = 5 * time.Second

	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"
	refreshGrant    = "refresh_token"
)

// OAuthApp is the OAuth side of a GitHub App: its client ID and the device
// flow switch from the App's settings.
type OAuthApp struct {
	ClientID   string
	DeviceFlow bool
}

// UserTokenKind says which half of a token pair an IssuedUserToken is.
type UserTokenKind string

// The two kinds of user token.
const (
	AccessToken  UserTokenKind = "access"
	RefreshToken UserTokenKind = "refresh"
)

// IssuedUserToken is a user access token or refresh token the fake handed
// out, recorded for leak searches.
type IssuedUserToken struct {
	Token     string        `json:"token"`
	Kind      UserTokenKind `json:"kind"`
	Login     string        `json:"login"`
	ClientID  string        `json:"client_id"`
	ExpiresAt time.Time     `json:"expires_at"`
}

// SSHSigningKey is an SSH signing key on a user's account.
type SSHSigningKey struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

type deviceCode struct {
	clientID  string
	userCode  string
	expiresAt time.Time
	interval  time.Duration
	lastPoll  time.Time
	login     string
	approved  bool
	denied    bool
}

type refreshRecord struct {
	login     string
	clientID  string
	expiresAt time.Time
	used      bool
}

type accessRecord struct {
	login     string
	expiresAt time.Time
}

// userState is the fake's OAuth and user-account state. The Server's mutex
// guards it.
type userState struct {
	apps            map[string]*OAuthApp
	devices         map[string]*deviceCode // by device code
	refresh         map[string]*refreshRecord
	access          map[string]*accessRecord
	issued          []IssuedUserToken
	signingKeys     map[string][]SSHSigningKey // by lower-case login
	accessLifetime  time.Duration
	refreshLifetime time.Duration
	deviceInterval  time.Duration
	reuses          []string
	onReuse         func(refreshToken string)
}

func newUserState() *userState {
	return &userState{
		apps:            map[string]*OAuthApp{},
		devices:         map[string]*deviceCode{},
		refresh:         map[string]*refreshRecord{},
		access:          map[string]*accessRecord{},
		signingKeys:     map[string][]SSHSigningKey{},
		accessLifetime:  defaultUserAccessLifetime,
		refreshLifetime: defaultUserRefreshLifetime,
		deviceInterval:  defaultDeviceInterval,
	}
}

func randomHex(prefix string, size int) string {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}

	return prefix + hex.EncodeToString(raw)
}

func newUserCode() string {
	const alphabet = "BCDFGHJKLMNPQRSTVWXZ"

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}

	out := make([]byte, 0, 9)

	for index, value := range raw {
		if index == 4 {
			out = append(out, '-')
		}

		out = append(out, alphabet[int(value)%len(alphabet)])
	}

	return string(out)
}

// CreateOAuthApp registers the client ID of an App, with the device flow
// switch of its settings.
func (s *Server) CreateOAuthApp(app OAuthApp) {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := app
	s.users.apps[app.ClientID] = &copied
}

// SetDeviceFlow turns the device flow of an App on or off. Off, the device
// code endpoint answers device_flow_disabled.
func (s *Server) SetDeviceFlow(clientID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	app, ok := s.users.apps[clientID]
	if !ok {
		return fmt.Errorf("oauth app %q does not exist", clientID)
	}

	app.DeviceFlow = enabled

	return nil
}

// SetUserTokenLifetimes sets the lifetimes of the user access and refresh
// tokens issued from now on; a value of zero or less restores GitHub's 8 hours
// and 6 months.
func (s *Server) SetUserTokenLifetimes(access, refresh time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if access <= 0 {
		access = defaultUserAccessLifetime
	}

	if refresh <= 0 {
		refresh = defaultUserRefreshLifetime
	}

	s.users.accessLifetime, s.users.refreshLifetime = access, refresh
}

// SetDeviceInterval sets the minimum polling interval of the device codes
// issued from now on; zero never answers slow_down. GitHub's is 5 seconds.
func (s *Server) SetDeviceInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.users.deviceInterval = max(d, 0)
}

// ApproveDeviceCode approves the user code as the login, as the person does
// at the verification page.
func (s *Server) ApproveDeviceCode(userCode, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceByUserCode(userCode)
	if device == nil {
		return fmt.Errorf("user code %q does not exist", userCode)
	}

	device.approved, device.login = true, login

	return nil
}

// DenyDeviceCode makes the person refuse the user code at the verification page.
func (s *Server) DenyDeviceCode(userCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceByUserCode(userCode)
	if device == nil {
		return fmt.Errorf("user code %q does not exist", userCode)
	}

	device.denied = true

	return nil
}

func (s *Server) deviceByUserCode(userCode string) *deviceCode {
	for _, device := range s.users.devices {
		if strings.EqualFold(device.userCode, userCode) {
			return device
		}
	}

	return nil
}

// OnRefreshReuse sets a function called with each refresh token that is sent
// again after its refresh. The function runs without the fake's lock held.
func (s *Server) OnRefreshReuse(fn func(refreshToken string)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.users.onReuse = fn
}

// RefreshReuses returns every refresh token that was sent again after its
// refresh.
func (s *Server) RefreshReuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.users.reuses...)
}

// UserTokens returns every user access token and refresh token issued so far.
func (s *Server) UserTokens() []IssuedUserToken {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]IssuedUserToken(nil), s.users.issued...)
}

// SSHSigningKeys returns the signing keys on a user's account.
func (s *Server) SSHSigningKeys(login string) []SSHSigningKey {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]SSHSigningKey(nil), s.users.signingKeys[strings.ToLower(login)]...)
}

// oauthParams reads the form body, falling back to the query like GitHub.
func oauthParams(request *http.Request) url.Values {
	_ = request.ParseForm()

	return request.Form
}

// writeOAuth answers like github.com's OAuth endpoints: form-encoded unless
// the client asks for JSON, and errors come with status 200 and an `error`
// field.
func writeOAuth(writer http.ResponseWriter, request *http.Request, fields map[string]any) {
	if strings.Contains(request.Header.Get("Accept"), "application/json") {
		writeJSON(writer, http.StatusOK, fields)

		return
	}

	values := url.Values{}
	for key, value := range fields {
		values.Set(key, fmt.Sprint(value))
	}

	writer.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(values.Encode()))
}

func writeOAuthError(writer http.ResponseWriter, request *http.Request, code, description string) {
	writeOAuth(writer, request, map[string]any{"error": code, "error_description": description})
}

func (s *Server) handleDeviceCode(writer http.ResponseWriter, request *http.Request) {
	params := oauthParams(request)

	s.mu.Lock()

	app, ok := s.users.apps[params.Get("client_id")]
	if !ok {
		s.mu.Unlock()
		writeOAuthError(writer, request, "incorrect_client_credentials", "The client_id and/or client_secret passed are incorrect.")

		return
	}

	if !app.DeviceFlow {
		s.mu.Unlock()
		writeOAuthError(writer, request, "device_flow_disabled", "Device Flow must be explicitly enabled for this App")

		return
	}

	now := s.nowLocked()
	code := randomHex("", 20)
	device := &deviceCode{
		clientID:  app.ClientID,
		userCode:  newUserCode(),
		expiresAt: now.Add(defaultDeviceCodeLifetime),
		interval:  s.users.deviceInterval,
	}
	s.users.devices[code] = device
	s.mu.Unlock()

	writeOAuth(writer, request, map[string]any{
		"device_code":      code,
		"user_code":        device.userCode,
		"verification_uri": hostURL(request) + "/login/device",
		"expires_in":       int(defaultDeviceCodeLifetime / time.Second),
		"interval":         int((device.interval + time.Second - 1) / time.Second),
	})
}

func (s *Server) handleOAuthAccessToken(writer http.ResponseWriter, request *http.Request) {
	params := oauthParams(request)

	switch params.Get("grant_type") {
	case deviceCodeGrant:
		s.deviceCodeGrant(writer, request, params)
	case refreshGrant:
		s.refreshGrant(writer, request, params)
	default:
		writeOAuthError(writer, request, "unsupported_grant_type", "The grant type is not supported.")
	}
}

func (s *Server) deviceCodeGrant(writer http.ResponseWriter, request *http.Request, params url.Values) {
	s.mu.Lock()

	device, ok := s.users.devices[params.Get("device_code")]
	if !ok || device.clientID != params.Get("client_id") {
		s.mu.Unlock()
		writeOAuthError(writer, request, "bad_verification_code", "The device_code provided is not valid.")

		return
	}

	now := s.nowLocked()

	switch {
	case !now.Before(device.expiresAt):
		s.mu.Unlock()
		writeOAuthError(writer, request, "expired_token", "The device_code has expired.")

		return
	case device.denied:
		s.mu.Unlock()
		writeOAuthError(writer, request, "access_denied", "The authorization request was denied.")

		return
	case !device.lastPoll.IsZero() && now.Sub(device.lastPoll) < device.interval:
		device.interval += slowDownIncrement
		device.lastPoll = now
		interval := int(device.interval / time.Second)

		s.mu.Unlock()
		writeOAuth(writer, request, map[string]any{
			"error":             "slow_down",
			"error_description": "Too many requests have been made in the same timeframe.",
			"interval":          interval,
		})

		return
	}

	device.lastPoll = now

	if !device.approved {
		s.mu.Unlock()
		writeOAuthError(writer, request, "authorization_pending", "The authorization request is still pending.")

		return
	}

	delete(s.users.devices, params.Get("device_code"))
	fields := s.issueUserTokensLocked(device.login, device.clientID)
	s.mu.Unlock()

	writeOAuth(writer, request, fields)
}

// refreshGrant exchanges a refresh token for a new pair. A refresh token works
// once; sending it again is refused with bad_refresh_token:
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens
func (s *Server) refreshGrant(writer http.ResponseWriter, request *http.Request, params url.Values) {
	s.mu.Lock()

	if _, ok := s.users.apps[params.Get("client_id")]; !ok {
		s.mu.Unlock()
		writeOAuthError(writer, request, "incorrect_client_credentials", "The client_id and/or client_secret passed are incorrect.")

		return
	}

	token := params.Get("refresh_token")
	record, ok := s.users.refresh[token]

	if !ok || record.clientID != params.Get("client_id") || !s.nowLocked().Before(record.expiresAt) {
		s.mu.Unlock()
		writeOAuthError(writer, request, "bad_refresh_token", "The refresh token passed is incorrect or expired.")

		return
	}

	if record.used {
		s.users.reuses = append(s.users.reuses, token)
		onReuse := s.users.onReuse
		s.mu.Unlock()

		if onReuse != nil {
			onReuse(token)
		}

		writeOAuthError(writer, request, "bad_refresh_token", "The refresh token passed is incorrect or expired.")

		return
	}

	record.used = true
	fields := s.issueUserTokensLocked(record.login, record.clientID)
	s.mu.Unlock()

	writeOAuth(writer, request, fields)
}

// issueUserTokensLocked mints a pair of user tokens and records them. The
// caller holds the lock.
func (s *Server) issueUserTokensLocked(login, clientID string) map[string]any {
	now := s.nowLocked()
	access := randomHex("ghu_", 20)
	refresh := randomHex("ghr_", 20)
	accessExpiry := now.Add(s.users.accessLifetime)
	refreshExpiry := now.Add(s.users.refreshLifetime)

	s.users.access[access] = &accessRecord{login: login, expiresAt: accessExpiry}
	s.users.refresh[refresh] = &refreshRecord{login: login, clientID: clientID, expiresAt: refreshExpiry}
	s.users.issued = append(s.users.issued,
		IssuedUserToken{Token: access, Kind: AccessToken, Login: login, ClientID: clientID, ExpiresAt: accessExpiry},
		IssuedUserToken{Token: refresh, Kind: RefreshToken, Login: login, ClientID: clientID, ExpiresAt: refreshExpiry},
	)

	return map[string]any{
		"access_token":             access,
		"expires_in":               int(s.users.accessLifetime / time.Second),
		"refresh_token":            refresh,
		"refresh_token_expires_in": int(s.users.refreshLifetime / time.Second),
		"token_type":               "bearer",
		"scope":                    "",
	}
}

// userHandler serves an endpoint that only a user access token may call. An
// installation token gets 403 "Resource not accessible by integration", as on
// GitHub; anything else unknown or expired gets 401.
func (s *Server) userHandler(next func(writer http.ResponseWriter, request *http.Request, login string)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		raw, ok := bearer(request)
		if !ok {
			writeJSONError(writer, http.StatusUnauthorized, "Requires authentication")

			return
		}

		s.mu.Lock()
		record := s.users.access[raw]
		live := record != nil && s.nowLocked().Before(record.expiresAt)
		s.mu.Unlock()

		if live {
			writer.Header().Set("X-RateLimit-Limit", "5000")
			writer.Header().Set("X-RateLimit-Remaining", "4999")
			next(writer, request, record.login)

			return
		}

		if _, isInstallation := s.lookupToken(raw); isInstallation {
			writeJSONError(writer, http.StatusForbidden, "Resource not accessible by integration")

			return
		}

		writeJSONError(writer, http.StatusUnauthorized, "Bad credentials")
	}
}

func (s *Server) handleUser(writer http.ResponseWriter, _ *http.Request, login string) {
	writeJSON(writer, http.StatusOK, map[string]any{"login": login, "type": "User"})
}

func (s *Server) handleListSSHSigningKeys(writer http.ResponseWriter, _ *http.Request, login string) {
	writeJSON(writer, http.StatusOK, append([]SSHSigningKey{}, s.SSHSigningKeys(login)...))
}

func (s *Server) handleCreateSSHSigningKey(writer http.ResponseWriter, request *http.Request, login string) {
	var body struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}
	if !decode(writer, request, &body) {
		return
	}

	if strings.TrimSpace(body.Key) == "" {
		writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed")

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	owner := strings.ToLower(login)

	for _, existing := range s.users.signingKeys[owner] {
		if existing.Key == body.Key {
			writeJSONError(writer, http.StatusUnprocessableEntity, "Validation Failed: key is already in use")

			return
		}
	}

	s.nextID++
	created := SSHSigningKey{ID: s.nextID, Title: body.Title, Key: body.Key, CreatedAt: s.nowLocked().UTC()}
	s.users.signingKeys[owner] = append(s.users.signingKeys[owner], created)

	writeJSON(writer, http.StatusCreated, created)
}

func (s *Server) handleGetSSHSigningKey(writer http.ResponseWriter, request *http.Request, login string) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err == nil {
		for _, key := range s.SSHSigningKeys(login) {
			if key.ID == id {
				writeJSON(writer, http.StatusOK, key)

				return
			}
		}
	}

	writeJSONError(writer, http.StatusNotFound, "Not Found")
}

func (s *Server) handleDeleteSSHSigningKey(writer http.ResponseWriter, request *http.Request, login string) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err == nil {
		s.mu.Lock()
		defer s.mu.Unlock()

		owner := strings.ToLower(login)

		for index, key := range s.users.signingKeys[owner] {
			if key.ID == id {
				s.users.signingKeys[owner] = append(s.users.signingKeys[owner][:index:index], s.users.signingKeys[owner][index+1:]...)

				writer.WriteHeader(http.StatusNoContent)

				return
			}
		}
	}

	writeJSONError(writer, http.StatusNotFound, "Not Found")
}
