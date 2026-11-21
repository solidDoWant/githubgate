package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	// ReasonRefreshTokenRevoked means GitHub refused the stored refresh
	// token; the user has to authorize the App again.
	ReasonRefreshTokenRevoked = "RefreshTokenRevoked"

	// refreshBefore is how long before the access token expires the leader
	// refreshes it: half of GitHub's 8 hours.
	refreshBefore = 4 * time.Hour
	// UserTokenExpirySkew is how close to its expiry a stored access token
	// counts as missing, so a request never leaves with a token about to die.
	UserTokenExpirySkew = 5 * time.Minute

	// The first and the longest wait between failed refreshes.
	refreshRetryFirst = time.Second
	refreshRetryMax   = 5 * time.Minute
)

// The results of a refresh, as the refresh metric counts them.
const (
	refreshOK       = "ok"
	refreshFailed   = "failed"
	refreshRevoked  = "revoked"
	refreshConflict = "conflict"
	// refreshNotDue means the fresh read found a token with enough time left
	// or no usable token; nothing was sent to GitHub.
	refreshNotDue = ""
)

// tokenKeys are the Secret keys cleared when the stored pair can't be used
// any more.
var tokenKeys = []string{KeyAccessToken, KeyAccessTokenExpiresAt, KeyRefreshToken, KeyRefreshTokenExpires}

// Metrics are the metrics of the user authorizations. Their labels never
// carry a token, a user code or a login.
type Metrics struct {
	ready         *prometheus.GaugeVec
	refreshes     *prometheus.CounterVec
	refreshExpiry *prometheus.GaugeVec
}

// NewMetrics returns the metrics, ready to be registered with Collectors.
func NewMetrics() *Metrics {
	return &Metrics{
		ready: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "githubgate_user_authorization_ready",
			Help: "1 when the user authorization is Ready, 0 otherwise.",
		}, []string{"namespace", "name"}),
		refreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "githubgate_user_token_refreshes_total",
			Help: "User token refreshes, by result (ok, failed, revoked or conflict).",
		}, []string{"namespace", "name", "result"}),
		refreshExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "githubgate_user_refresh_token_expiry_seconds",
			Help: "When the stored refresh token expires, in seconds since the Unix epoch.",
		}, []string{"namespace", "name"}),
	}
}

// Collectors returns the collectors to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.ready, m.refreshes, m.refreshExpiry}
}

func (m *Metrics) observe(name types.NamespacedName, status *githubgatev1alpha1.GitHubUserAuthorizationStatus) {
	ready := 0.0
	if readyReason(status.Conditions) == "Ready" {
		ready = 1
	}

	m.ready.WithLabelValues(name.Namespace, name.Name).Set(ready)

	if status.RefreshTokenExpiresAt != nil {
		m.refreshExpiry.WithLabelValues(name.Namespace, name.Name).Set(float64(status.RefreshTokenExpiresAt.Unix()))
	} else {
		m.refreshExpiry.DeleteLabelValues(name.Namespace, name.Name)
	}
}

func (m *Metrics) forget(name types.NamespacedName) {
	m.ready.DeleteLabelValues(name.Namespace, name.Name)
	m.refreshExpiry.DeleteLabelValues(name.Namespace, name.Name)
	m.refreshes.DeletePartialMatch(prometheus.Labels{"namespace": name.Namespace, "name": name.Name})
}

// workers serializes the token writes of one authorization: the device
// flow's store, the refresh and the clear never run at the same time.
type workers struct {
	mu       sync.Mutex
	locks    map[types.NamespacedName]*sync.Mutex
	failures map[types.NamespacedName]int
}

func newWorkers() *workers {
	return &workers{locks: map[types.NamespacedName]*sync.Mutex{}, failures: map[types.NamespacedName]int{}}
}

func (w *workers) lock(name types.NamespacedName) func() {
	w.mu.Lock()

	lock, ok := w.locks[name]
	if !ok {
		lock = &sync.Mutex{}
		w.locks[name] = lock
	}

	w.mu.Unlock()

	lock.Lock()

	return lock.Unlock
}

// failed counts a failed refresh and returns how long to wait before the
// next one.
func (w *workers) failed(name types.NamespacedName) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()

	delay := refreshRetryFirst << min(w.failures[name], 10)
	w.failures[name]++

	return min(delay, refreshRetryMax)
}

func (w *workers) succeeded(name types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.failures, name)
}

// refreshOutcome is what refreshIfDue did.
type refreshOutcome struct {
	result string
	// tokens is the pair now stored, for refreshOK and refreshNotDue.
	tokens userTokens
	// state is the fresh read's state, for refreshNotDue.
	state tokenState
	// reason and message describe a revoked or unusable pair, or a failure.
	reason, message string
}

// refreshIfDue refreshes object's token pair when its access token has less
// than refreshBefore left, or at once when force is set. It decides from an uncached read, so it never
// spends a refresh token that another write already replaced.
func (r *userAuthorizationReconciler) refreshIfDue(
	ctx context.Context,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	authorization *model.UserAuthorization,
	force bool,
) refreshOutcome {
	name := client.ObjectKeyFromObject(object)

	unlock := r.workers.lock(name)
	defer unlock()

	secret, state, tokens, err := getUserTokens(ctx, r.apiReader, object, authorization)
	if err != nil {
		return refreshOutcome{result: refreshFailed, message: "the token Secret could not be read"}
	}

	if state != tokenValid || (!force && time.Until(tokens.accessTokenExpiresAt) >= refreshBefore) {
		return refreshOutcome{result: refreshNotDue, state: state, tokens: tokens}
	}

	sent := tokens.refreshToken

	callContext, cancel := context.WithTimeout(r.oauthContext(ctx), oauthCallTimeout)
	defer cancel()

	// One refresh grant, called once: an empty access token makes the
	// source refresh, and the source is dropped afterwards.
	token, err := r.oauthConfig(authorization.ClientID).TokenSource(callContext, &oauth2.Token{RefreshToken: sent}).Token()
	if err != nil {
		code, description := oauthFailure(err)
		if code != "bad_refresh_token" {
			return refreshOutcome{result: refreshFailed, tokens: tokens, message: description}
		}

		return r.clearTokens(ctx, object, secret, sent, refreshRevoked, ReasonRefreshTokenRevoked,
			"GitHub refused the refresh token; the stored tokens were cleared; set the annotation to authorize again")
	}

	refreshExpiresIn := extraSeconds(token.Extra("refresh_token_expires_in"))
	if token.Expiry.IsZero() || token.RefreshToken == "" || token.RefreshToken == sent || refreshExpiresIn <= 0 {
		// The old pair is spent either way, so it is cleared too.
		return r.clearTokens(ctx, object, secret, sent, refreshRevoked, ReasonTokenExpiryDisabled,
			"GitHub returned a token that does not expire; turn on user-to-server token expiration in the App's settings and set the annotation again")
	}

	refreshed := tokens
	refreshed.accessToken = token.AccessToken
	refreshed.accessTokenExpiresAt = token.Expiry.UTC().Truncate(time.Second)
	refreshed.refreshToken = token.RefreshToken
	refreshed.refreshTokenExpiresAt = time.Now().Add(time.Duration(refreshExpiresIn) * time.Second).UTC().Truncate(time.Second)

	result, err := r.writeTokens(ctx, object, secret, sent, func(current *corev1.Secret) {
		current.Data = refreshed.data()
	})
	if err != nil {
		// The pair is lost: the old refresh token no longer works, and the
		// next refresh is refused.
		return refreshOutcome{result: refreshFailed, tokens: tokens, message: "the refreshed tokens could not be stored"}
	}

	return refreshOutcome{result: result, tokens: refreshed}
}

// clearTokens removes the token keys from the copy that held the refused
// refresh token, and reports reason unless a newer pair was written.
func (r *userAuthorizationReconciler) clearTokens(
	ctx context.Context,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	secret *corev1.Secret,
	sent, result, reason, message string,
) refreshOutcome {
	written, err := r.writeTokens(ctx, object, secret, sent, removeTokenKeys)

	switch {
	case err != nil:
		// The refused pair is still stored and never works; the status says
		// so anyway, and the next reconcile tries the clear again.
		return refreshOutcome{result: result, reason: reason, message: message}
	case written == refreshConflict:
		return refreshOutcome{result: refreshConflict}
	default:
		return refreshOutcome{result: result, reason: reason, message: message}
	}
}

// clearSpecChanged removes the token keys when the stored pair was
// authorized for another login, App or client ID, so a later revert of the
// spec never brings the old pair back. It decides from an uncached read and
// clears on that copy's resourceVersion; a pair written in between is kept,
// and the next reconcile decides again.
func (r *userAuthorizationReconciler) clearSpecChanged(
	ctx context.Context,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	authorization *model.UserAuthorization,
) error {
	unlock := r.workers.lock(client.ObjectKeyFromObject(object))
	defer unlock()

	secret, state, tokens, err := getUserTokens(ctx, r.apiReader, object, authorization)
	if err != nil || state != tokenSpecChanged {
		return err
	}

	_, err = r.writeTokens(ctx, object, secret, tokens.refreshToken, removeTokenKeys)

	return err
}

// removeTokenKeys removes the token keys from a token Secret, keeping what
// the pair was bound to.
func removeTokenKeys(current *corev1.Secret) {
	data := maps.Clone(current.Data)
	for _, key := range tokenKeys {
		delete(data, key)
	}

	current.Data = data
}

// writeTokens changes the token Secret on the resourceVersion of secret, the
// copy that held the refresh token sent. On a conflict it reads again: if the
// stored refresh token is no longer sent, someone newer wrote, and the
// change is dropped (refreshConflict). Other failures are retried with
// backoff for up to storeTimeout.
func (r *userAuthorizationReconciler) writeTokens(
	ctx context.Context,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	secret *corev1.Secret,
	sent string,
	change func(*corev1.Secret),
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	current := secret.DeepCopy()
	delay := 100 * time.Millisecond

	for {
		change(current)

		err := r.client.Update(ctx, current)
		if err == nil {
			return refreshOK, nil
		}

		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			var fresh corev1.Secret

			readErr := r.apiReader.Get(ctx, client.ObjectKeyFromObject(secret), &fresh)
			if apierrors.IsNotFound(readErr) {
				return refreshConflict, nil
			}

			if readErr == nil {
				if !ownsUserToken(&fresh, object) || string(fresh.Data[KeyRefreshToken]) != sent {
					return refreshConflict, nil
				}

				current = &fresh

				continue
			}

			err = readErr
		}

		select {
		case <-ctx.Done():
			return refreshFailed, errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}

		delay = min(2*delay, 10*time.Second)
	}
}

// refreshStatus applies a refresh outcome to the status and says when to
// look again.
func (r *userAuthorizationReconciler) refreshStatus(
	name types.NamespacedName,
	status *githubgatev1alpha1.GitHubUserAuthorizationStatus,
	generation int64,
	outcome refreshOutcome,
) time.Duration {
	if outcome.result != refreshNotDue {
		r.metrics.refreshes.WithLabelValues(name.Namespace, name.Name, outcome.result).Inc()
	}

	switch outcome.result {
	case refreshOK:
		r.workers.succeeded(name)

		now := time.Now().UTC().Truncate(time.Second)
		status.LastRefreshTime = timeOrNil(now)
		setAuthorized(status, generation, outcome.tokens)

		return untilRefresh(outcome.tokens)
	case refreshNotDue:
		if outcome.state != tokenValid {
			// The Secret changed since the cached read; its watch event
			// brings the next reconcile.
			return time.Second
		}

		r.workers.succeeded(name)
		setAuthorized(status, generation, outcome.tokens)

		return untilRefresh(outcome.tokens)
	case refreshConflict:
		// A newer pair was written; the next reconcile reads it.
		return time.Second
	case refreshRevoked:
		r.workers.succeeded(name)

		status.AccessTokenExpiresAt = nil
		status.RefreshTokenExpiresAt = nil
		setOutcome(&status.Conditions, generation, outcome.reason, outcome.message)

		return 0
	default:
		retry := r.workers.failed(name)
		message := "RefreshFailed: the user token could not be refreshed: " + outcome.message + "; retrying"

		if time.Until(outcome.tokens.accessTokenExpiresAt) > UserTokenExpirySkew {
			setAuthorized(status, generation, outcome.tokens)
			setReady(&status.Conditions, generation, true, model.ReasonReady,
				fmt.Sprintf("a user token of %s is stored; %s", outcome.tokens.login, message))
		} else {
			setReady(&status.Conditions, generation, false, ReasonGitHubUnavailable, "the access token expired; "+message)
		}

		return retry
	}
}

// untilRefresh is how long until tokens are due for a refresh.
func untilRefresh(tokens userTokens) time.Duration {
	return max(time.Until(tokens.accessTokenExpiresAt.Add(-refreshBefore)), time.Second)
}

// UserAccessToken returns the access token stored for authorization, read
// through reader. Serving passes the manager's cached client, so every
// replica sees a refresh within the watch latency; no replica refreshes on
// its own. A token that is missing, not bound to the authorization's spec or
// within UserTokenExpirySkew of its expiry returns false.
func UserAccessToken(ctx context.Context, reader client.Reader, authorization *model.UserAuthorization) (string, bool, error) {
	var object githubgatev1alpha1.GitHubUserAuthorization
	if err := reader.Get(ctx, authorization.NamespacedName, &object); err != nil {
		return "", false, client.IgnoreNotFound(err)
	}

	_, state, tokens, err := getUserTokens(ctx, reader, &object, authorization)
	if err != nil || state != tokenValid || tokens.accessToken == "" {
		return "", false, err
	}

	if time.Until(tokens.accessTokenExpiresAt) <= UserTokenExpirySkew {
		return "", false, nil
	}

	return tokens.accessToken, true, nil
}
