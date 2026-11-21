package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gogithub "github.com/google/go-github/v93/github"
	"golang.org/x/oauth2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	// AnnotationAuthorize starts a device flow when set to a value that
	// wasn't handled before.
	AnnotationAuthorize = "githubgate.soliddowant.dev/authorize"

	// Reasons of a user authorization's conditions.
	ReasonAuthorizationPending = "AuthorizationPending"
	ReasonAuthorizationExpired = "AuthorizationExpired"
	ReasonAuthorizationDenied  = "AuthorizationDenied"
	ReasonDeviceFlowDisabled   = "DeviceFlowDisabled"
	ReasonTokenExpiryDisabled  = "TokenExpiryDisabled"
	ReasonWrongUser            = "WrongUser"
	ReasonFlowLost             = "FlowLost"
	ReasonSpecChanged          = "SpecChanged"
	ReasonAuthorized           = "Authorized"

	// DefaultOAuthURL is where GitHub serves the device flow.
	DefaultOAuthURL = "https://github.com"

	// oauthCallTimeout bounds one OAuth or REST call outside the polling.
	oauthCallTimeout = 30 * time.Second
	// storeTimeout bounds the retries of writing a new token pair.
	storeTimeout = 2 * time.Minute
)

// flowOutcomes are the reasons a finished flow can end in; both conditions
// take the same reason, and the reconciler keeps them until something new
// happens.
var flowOutcomes = map[string]bool{
	ReasonAuthorizationExpired: true,
	ReasonAuthorizationDenied:  true,
	ReasonDeviceFlowDisabled:   true,
	ReasonTokenExpiryDisabled:  true,
	ReasonWrongUser:            true,
	ReasonFlowLost:             true,
	ReasonRefreshTokenRevoked:  true,
}

// flow is a device flow in progress on this leader. The device code lives
// only in its goroutine.
type flow struct {
	request string
	cancel  context.CancelFunc
}

// flows holds the device flows of this leader. It is a leader-elected
// runnable, so its context ends, and every flow with it, when leadership
// does.
type flows struct {
	mu      sync.Mutex
	ctx     context.Context
	running map[types.NamespacedName]*flow
}

func newFlows() *flows { return &flows{running: map[types.NamespacedName]*flow{}} }

// NeedLeaderElection reports that flows run only on the leader.
func (f *flows) NeedLeaderElection() bool { return true }

// Start keeps the flows' context until ctx ends.
func (f *flows) Start(ctx context.Context) error {
	f.mu.Lock()
	f.ctx = ctx
	f.mu.Unlock()

	<-ctx.Done()

	return nil
}

// started reports whether the leader started the flows.
func (f *flows) started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ctx != nil
}

// begin replaces the flow of name with a new one for request and returns its
// context, or false when the leader hasn't started yet.
func (f *flows) begin(name types.NamespacedName, request string) (context.Context, *flow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ctx == nil {
		return nil, nil, false
	}

	if previous, ok := f.running[name]; ok {
		previous.cancel()
	}

	ctx, cancel := context.WithCancel(f.ctx)
	current := &flow{request: request, cancel: cancel}
	f.running[name] = current

	return ctx, current, true
}

// stop ends the flow of name, if any.
func (f *flows) stop(name types.NamespacedName) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if previous, ok := f.running[name]; ok {
		previous.cancel()
		delete(f.running, name)
	}
}

// finish forgets current once its goroutine is done.
func (f *flows) finish(name types.NamespacedName, current *flow) {
	f.mu.Lock()
	defer f.mu.Unlock()

	current.cancel()

	if f.running[name] == current {
		delete(f.running, name)
	}
}

// runs reports whether this leader holds the flow of name for request.
func (f *flows) runs(name types.NamespacedName, request string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	current, ok := f.running[name]

	return ok && current.request == request
}

// oauthErrorTransport makes GitHub's OAuth errors, which come with HTTP 200
// and an error field, look like the error statuses golang.org/x/oauth2
// parses. The device code endpoint's errors are otherwise taken for an empty
// device code.
type oauthErrorTransport struct{ next http.RoundTripper }

func (t oauthErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return response, err
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()

	if err != nil {
		return nil, err
	}

	response.Body = io.NopCloser(bytes.NewReader(body))

	var fields struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &fields) == nil && fields.Error != "" {
		response.StatusCode = http.StatusBadRequest
		response.Status = http.StatusText(http.StatusBadRequest)
	}

	return response, nil
}

// oauthFailure reduces an OAuth failure to its HTTP status and OAuth error
// code. The error text can carry the response body, so it is never used.
func oauthFailure(err error) (code, description string) {
	var retrieveError *oauth2.RetrieveError
	if errors.As(err, &retrieveError) {
		status := 0
		if retrieveError.Response != nil {
			status = retrieveError.Response.StatusCode
		}

		return retrieveError.ErrorCode, fmt.Sprintf("GitHub answered HTTP %d, error %q", status, retrieveError.ErrorCode)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "expired_token", "the user code expired"
	}

	return "", "GitHub could not be reached"
}

func (r *userAuthorizationReconciler) oauthConfig(clientID string) *oauth2.Config {
	base := strings.TrimSuffix(r.options.OAuthURL, "/")
	if base == "" {
		base = DefaultOAuthURL
	}

	return &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: base + "/login/device/code",
			TokenURL:      base + "/login/oauth/access_token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

func (r *userAuthorizationReconciler) oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: oauthErrorTransport{next: r.transport()}})
}

func (r *userAuthorizationReconciler) transport() http.RoundTripper {
	if r.options.Transport != nil {
		return r.options.Transport
	}

	return http.DefaultTransport
}

// startFlow handles a new authorize annotation value: it records the value
// as handled first, so a crash can't start a second flow for it, then asks
// GitHub for a device code and starts polling.
func (r *userAuthorizationReconciler) startFlow(
	ctx context.Context,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	authorization *model.UserAuthorization,
	request string,
) error {
	name := client.ObjectKeyFromObject(object)

	if !r.flows.started() {
		return errors.New("the device flows have not started yet")
	}

	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	status.LastAuthorizeRequest = request
	status.Authorization = nil

	// The write is conditional on the object just read, so a reconcile of a
	// stale copy fails here and never replaces a newer flow.
	if err := update(ctx, r.client, object, &object.Status, status); err != nil {
		return err
	}

	flowContext, current, ok := r.flows.begin(name, request)
	if !ok {
		return errors.New("the device flows have stopped")
	}

	config := r.oauthConfig(authorization.ClientID)

	callContext, cancel := context.WithTimeout(r.oauthContext(ctx), oauthCallTimeout)
	defer cancel()

	device, err := config.DeviceAuth(callContext)
	if err == nil && device.DeviceCode == "" {
		err = errors.New("GitHub returned no device code")
	}

	if err != nil {
		r.flows.finish(name, current)

		code, description := oauthFailure(err)

		reason, message := ReasonFlowLost, "the device flow could not be started: "+description+"; set the annotation again"
		if code == "device_flow_disabled" {
			reason, message = ReasonDeviceFlowDisabled, "the App's device flow is off; turn it on in the App's settings and set the annotation again"
		}

		return r.finishStatus(ctx, name, request, func(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64) {
			setOutcome(&status.Conditions, generation, reason, message)
		})
	}

	err = r.writeStatus(ctx, name, func(object *githubgatev1alpha1.GitHubUserAuthorization) bool {
		if object.Status.LastAuthorizeRequest != request {
			return false
		}

		object.Status.Authorization = &githubgatev1alpha1.DeviceAuthorizationStatus{
			UserCode:        device.UserCode,
			VerificationURI: device.VerificationURI,
			ExpiresAt:       metav1.NewTime(device.Expiry.UTC().Truncate(time.Second)),
			Request:         request,
		}
		setOutcome(&object.Status.Conditions, object.Generation, ReasonAuthorizationPending,
			"waiting for "+authorization.Login+" to enter the user code at the verification URI")

		return true
	})
	if err != nil {
		r.flows.finish(name, current)
		return err
	}

	go r.poll(flowContext, current, name, authorization, config, device)

	return nil
}

// poll waits for the user's answer and stores the token. The device code
// never leaves this goroutine.
func (r *userAuthorizationReconciler) poll(
	ctx context.Context,
	current *flow,
	name types.NamespacedName,
	authorization *model.UserAuthorization,
	config *oauth2.Config,
	device *oauth2.DeviceAuthResponse,
) {
	defer r.flows.finish(name, current)

	token, err := config.DeviceAccessToken(r.oauthContext(ctx), device)
	if ctx.Err() != nil {
		// A newer flow replaced this one, or this replica stopped leading;
		// either way the status belongs to someone else now.
		return
	}

	var apply func(*githubgatev1alpha1.GitHubUserAuthorizationStatus, int64)

	if err != nil {
		apply = flowFailure(err)
	} else {
		apply = r.accept(ctx, name, authorization, token)
	}

	if ctx.Err() != nil {
		return
	}

	if err := r.finishStatus(ctx, name, current.request, apply); err != nil && ctx.Err() == nil {
		r.log.Error(err, "writing the end of a device flow", "authorization", name.String())
	}
}

func flowFailure(err error) func(*githubgatev1alpha1.GitHubUserAuthorizationStatus, int64) {
	code, description := oauthFailure(err)

	reason, message := ReasonFlowLost, "the device flow failed: "+description+"; set the annotation again"

	switch code {
	case "expired_token":
		reason, message = ReasonAuthorizationExpired, "the user code expired before it was approved; set the annotation again"
	case "access_denied":
		reason, message = ReasonAuthorizationDenied, "the user code was denied; set the annotation again"
	}

	return func(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64) {
		setOutcome(&status.Conditions, generation, reason, message)
	}
}

// accept checks a token from the device flow and stores it. A token that
// doesn't expire, or that belongs to another user, is discarded.
func (r *userAuthorizationReconciler) accept(
	ctx context.Context,
	name types.NamespacedName,
	authorization *model.UserAuthorization,
	token *oauth2.Token,
) func(*githubgatev1alpha1.GitHubUserAuthorizationStatus, int64) {
	outcome := func(reason, message string) func(*githubgatev1alpha1.GitHubUserAuthorizationStatus, int64) {
		return func(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64) {
			setOutcome(&status.Conditions, generation, reason, message)
		}
	}

	refreshExpiresIn := extraSeconds(token.Extra("refresh_token_expires_in"))
	if token.Expiry.IsZero() || token.RefreshToken == "" || refreshExpiresIn <= 0 {
		return outcome(ReasonTokenExpiryDisabled,
			"GitHub returned a token that does not expire; turn on user-to-server token expiration in the App's settings and set the annotation again")
	}

	login, err := r.login(ctx, token.AccessToken)
	if err != nil {
		return outcome(ReasonFlowLost, "the token's user could not be checked; set the annotation again")
	}

	if !strings.EqualFold(login, authorization.Login) {
		return outcome(ReasonWrongUser, fmt.Sprintf("the code was approved by GitHub user %q, not %q; the token was discarded", login, authorization.Login))
	}

	tokens := userTokens{
		accessToken:           token.AccessToken,
		accessTokenExpiresAt:  token.Expiry.UTC().Truncate(time.Second),
		refreshToken:          token.RefreshToken,
		refreshTokenExpiresAt: time.Now().Add(time.Duration(refreshExpiresIn) * time.Second).UTC().Truncate(time.Second),
		login:                 login,
		app:                   authorization.App.String(),
		clientID:              authorization.ClientID,
	}

	if err := r.store(ctx, name, tokens); err != nil {
		if errors.Is(err, errForeignSecret) {
			return func(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64) {
				setReady(&status.Conditions, generation, false, model.ReasonSecretInvalid, foreignSecretMessage(name.Name))
			}
		}

		return outcome(ReasonFlowLost, "the token could not be stored; set the annotation again")
	}

	return func(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64) {
		setAuthorized(status, generation, tokens)
	}
}

func foreignSecretMessage(name string) string {
	return fmt.Sprintf("Secret %s exists and was not created by githubgate for this authorization; it is never used or changed",
		UserTokenSecretName(name))
}

// store writes the token pair as soon as GitHub returned it, retrying with
// backoff, because a lost pair can't be refreshed.
func (r *userAuthorizationReconciler) store(ctx context.Context, name types.NamespacedName, tokens userTokens) error {
	unlock := r.workers.lock(name)
	defer unlock()

	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	delay := 100 * time.Millisecond

	for {
		var object githubgatev1alpha1.GitHubUserAuthorization

		err := r.apiReader.Get(ctx, name, &object)
		if err == nil {
			err = storeUserTokens(ctx, r.client, r.apiReader, &object, tokens)
		}

		if err == nil || errors.Is(err, errForeignSecret) {
			return err
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}

		delay = min(2*delay, 10*time.Second)
	}
}

// login asks GitHub whose token it is.
func (r *userAuthorizationReconciler) login(ctx context.Context, accessToken string) (string, error) {
	baseURL := r.options.APIURL
	if baseURL == "" {
		baseURL = "https://api.github.com/"
	}

	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}

	api, err := gogithub.NewClient(
		gogithub.WithTransport(r.transport()), gogithub.WithURLs(&baseURL, nil), gogithub.WithAuthToken(accessToken),
	)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, oauthCallTimeout)
	defer cancel()

	user, _, err := api.Users.Get(ctx, "")
	if err != nil {
		return "", errors.New("GET /user failed")
	}

	if user.GetLogin() == "" {
		return "", errors.New("GET /user returned no login")
	}

	return user.GetLogin(), nil
}

// extraSeconds reads a number of seconds from a token response field.
// golang.org/x/oauth2 gives a form-encoded number as int64 or float64, a JSON
// number as float64, and anything else as a string.
func extraSeconds(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	}

	return 0
}

// finishStatus ends the flow for request in the status, unless a newer
// request took over.
func (r *userAuthorizationReconciler) finishStatus(
	ctx context.Context,
	name types.NamespacedName,
	request string,
	apply func(*githubgatev1alpha1.GitHubUserAuthorizationStatus, int64),
) error {
	return r.writeStatus(ctx, name, func(object *githubgatev1alpha1.GitHubUserAuthorization) bool {
		if object.Status.LastAuthorizeRequest != request {
			return false
		}

		object.Status.Authorization = nil
		object.Status.ObservedGeneration = object.Generation
		apply(&object.Status, object.Generation)

		return true
	})
}

// writeStatus changes the status from a fresh read, retrying on conflicts.
// change returns false to write nothing.
func (r *userAuthorizationReconciler) writeStatus(
	ctx context.Context,
	name types.NamespacedName,
	change func(*githubgatev1alpha1.GitHubUserAuthorization) bool,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var object githubgatev1alpha1.GitHubUserAuthorization
		if err := r.apiReader.Get(ctx, name, &object); err != nil {
			return client.IgnoreNotFound(err)
		}

		if !change(&object) {
			return nil
		}

		return r.client.Status().Update(ctx, &object)
	})
}

// setOutcome sets both conditions to reason, for a flow that is pending or
// ended without a usable token.
func setOutcome(conditions *[]metav1.Condition, generation int64, reason, message string) {
	setReady(conditions, generation, false, reason, message)
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type: ConditionAuthorizationRequired, Status: metav1.ConditionTrue, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

// setAuthorized shows a usable stored token.
func setAuthorized(status *githubgatev1alpha1.GitHubUserAuthorizationStatus, generation int64, tokens userTokens) {
	status.Login = tokens.login
	status.AccessTokenExpiresAt = timeOrNil(tokens.accessTokenExpiresAt)
	status.RefreshTokenExpiresAt = timeOrNil(tokens.refreshTokenExpiresAt)

	setReady(&status.Conditions, generation, true, model.ReasonReady, "a user token of "+tokens.login+" is stored")
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: ConditionAuthorizationRequired, Status: metav1.ConditionFalse, Reason: ReasonAuthorized,
		Message: "the App is authorized as " + tokens.login, ObservedGeneration: generation,
	})
}

func timeOrNil(value time.Time) *metav1.Time {
	if value.IsZero() {
		return nil
	}

	return &metav1.Time{Time: value}
}
