//go:build integration

package controller_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/controller"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

// isRefreshGrant reports whether request asks the fake GitHub for a refresh.
func isRefreshGrant(request *http.Request) bool {
	if request.Method != http.MethodPost || request.URL.Path != "/login/oauth/access_token" {
		return false
	}

	_ = request.ParseForm()

	return request.PostForm.Get("grant_type") == "refresh_token"
}

// onRefresh answers or watches the refresh grants until the test ends.
func (e *env) onRefresh(t *testing.T, fn func(http.ResponseWriter, *http.Request) bool) {
	t.Helper()

	intercept := func(writer http.ResponseWriter, request *http.Request) bool {
		return isRefreshGrant(request) && fn(writer, request)
	}
	e.intercept.Store(&intercept)
	t.Cleanup(func() { e.intercept.Store(nil) })
}

func (e *env) tokenSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()

	var secret corev1.Secret
	require.NoError(t, e.client.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: controller.UserTokenSecretName(name)}, &secret))

	return &secret
}

// secretData reads the token Secret's data for a condition function, which
// can run after the test ended; nil when it can't be read.
func (e *env) secretData(t *testing.T, name string) map[string][]byte {
	var secret corev1.Secret
	if err := e.client.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: controller.UserTokenSecretName(name)}, &secret); err != nil {
		return nil
	}

	return secret.Data
}

// authorizeShortLived authorizes name as octocat with an access token that
// has less than four hours left from the start, and returns that token.
func (e *env) authorizeShortLived(t *testing.T, name string) (*githubgatev1alpha1.GitHubUserAuthorization, string) {
	t.Helper()

	return e.authorizeWithLifetime(t, name, 3*time.Hour)
}

// authorizeWithLifetime authorizes name through the device flow; the fake
// issues access tokens that last lifetime, or GitHub's 8 hours when zero.
func (e *env) authorizeWithLifetime(t *testing.T, name string, lifetime time.Duration) (*githubgatev1alpha1.GitHubUserAuthorization, string) {
	t.Helper()

	e.fake.SetUserTokenLifetimes(lifetime, 0)
	t.Cleanup(func() { e.fake.SetUserTokenLifetimes(0, 0) })

	object := deviceAuthorization(name)
	e.create(t, object)

	pending := e.requirePending(t, object)
	issuedBefore := len(e.fake.UserTokens())
	require.NoError(t, e.fake.ApproveDeviceCode(pending.UserCode, "octocat"))

	// The leader may refresh the first token before the test reads the
	// Secret, so the first token is taken from what the fake issued.
	var first string

	require.Eventually(t, func() bool {
		for _, issued := range e.fake.UserTokens()[issuedBefore:] {
			if issued.Kind == fakegithub.AccessToken {
				first = issued.Token
				return e.secretData(t, name) != nil
			}
		}

		return false
	}, 30*time.Second, 50*time.Millisecond, "waiting for the first token")

	return object, first
}

// servingView is the authorization as serving resolves it.
func servingView(object *githubgatev1alpha1.GitHubUserAuthorization) *model.UserAuthorization {
	return &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: object.Namespace, Name: object.Name},
		App:            &model.App{NamespacedName: types.NamespacedName{Namespace: adminNamespace, Name: "device"}},
		ClientID:       deviceClientID,
		Login:          object.Spec.Login,
		Resolved:       true,
	}
}

// replicaTokens returns the access token each replica would serve, "" for none.
func (e *env) replicaTokens(t *testing.T, object *githubgatev1alpha1.GitHubUserAuthorization) []string {
	t.Helper()

	tokens := make([]string, 0, len(e.readers))

	for _, reader := range e.readers {
		token, _, err := controller.UserAccessToken(t.Context(), reader, servingView(object))
		if err != nil {
			token = "error: " + err.Error()
		}

		tokens = append(tokens, token)
	}

	return tokens
}

// refreshCount sums the refresh metric of name with result over every replica.
func (e *env) refreshCount(t *testing.T, name, result string) float64 {
	t.Helper()

	total := 0.0

	for _, gatherer := range e.gatherers {
		families, err := gatherer.Gather()
		require.NoError(t, err)

		for _, family := range families {
			if family.GetName() != "githubgate_user_token_refreshes_total" {
				continue
			}

			for _, metric := range family.GetMetric() {
				if labels(metric)["name"] == name && labels(metric)["result"] == result {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}

	return total
}

func labels(metric *dto.Metric) map[string]string {
	out := map[string]string{}
	for _, pair := range metric.GetLabel() {
		out[pair.GetName()] = pair.GetValue()
	}

	return out
}

func readyCondition(t *testing.T, e *env, object *githubgatev1alpha1.GitHubUserAuthorization) *metav1.Condition {
	t.Helper()

	if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
		return nil
	}

	return apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionReady)
}

func TestUserTokenRefresh(t *testing.T) {
	e := startDeviceFlowEnv(t)

	t.Run("the leader refreshes a token with less than four hours left, every replica serves the new token, and no refresh token is used twice", func(t *testing.T) {
		e.fake.OnRefreshReuse(func(string) { t.Error("a refresh token was used twice") })
		t.Cleanup(func() { e.fake.OnRefreshReuse(nil) })

		object, first := e.authorizeShortLived(t, "refreshed")
		// The new pair lasts the full 8 hours, so it isn't refreshed again.
		e.fake.SetUserTokenLifetimes(0, 0)

		var current string

		require.Eventually(t, func() bool {
			data := e.secretData(t, object.Name)
			current = string(data[controller.KeyAccessToken])
			expiry, err := time.Parse(time.RFC3339, string(data[controller.KeyAccessTokenExpiresAt]))

			return err == nil && current != first && time.Until(expiry) > 7*time.Hour
		}, 30*time.Second, 50*time.Millisecond, "waiting for the refresh")

		require.Eventually(t, func() bool {
			for _, token := range e.replicaTokens(t, object) {
				if token != current {
					return false
				}
			}

			return true
		}, 30*time.Second, 50*time.Millisecond, "waiting for every replica to serve the new token")

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionTrue, "Ready")
		assert.NotNil(t, object.Status.LastRefreshTime)
		assert.GreaterOrEqual(t, e.refreshCount(t, object.Name, "ok"), 1.0)
		assert.Empty(t, e.fake.RefreshReuses())
	})

	t.Run("a refresh whose pair was replaced while GitHub answered keeps the newer pair", func(t *testing.T) {
		var replaced atomic.Bool

		// Another writer stores a newer pair between the leader's read and
		// its write, as a replica that lost leadership would see it.
		e.onRefresh(t, func(http.ResponseWriter, *http.Request) bool {
			if replaced.Swap(true) {
				return false
			}

			assert.NoError(t, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				secret := e.tokenSecret(t, "replaced")
				secret.Data[controller.KeyAccessToken] = []byte("ghu_newer")
				secret.Data[controller.KeyAccessTokenExpiresAt] = []byte(time.Now().Add(8 * time.Hour).UTC().Format(time.RFC3339))
				secret.Data[controller.KeyRefreshToken] = []byte("ghr_newer")

				return e.client.Update(t.Context(), secret)
			}))

			return false
		})

		object, _ := e.authorizeShortLived(t, "replaced")

		require.Eventually(t, func() bool {
			return e.refreshCount(t, object.Name, "conflict") >= 1
		}, 30*time.Second, 50*time.Millisecond, "waiting for the dropped refresh")

		assert.Never(t, func() bool {
			data := e.secretData(t, object.Name)

			return string(data[controller.KeyAccessToken]) != "ghu_newer" ||
				string(data[controller.KeyRefreshToken]) != "ghr_newer"
		}, 2*time.Second, 100*time.Millisecond, "the newer pair was overwritten")
		assert.Zero(t, e.refreshCount(t, object.Name, "revoked"))
	})

	t.Run("a refused refresh token clears the stored tokens and reports RefreshTokenRevoked", func(t *testing.T) {
		// GitHub answers OAuth errors with HTTP 200 and an error field.
		e.onRefresh(t, func(writer http.ResponseWriter, _ *http.Request) bool {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"error": "bad_refresh_token", "error_description": "The refresh token passed is incorrect or expired.",
			})

			return true
		})

		object, _ := e.authorizeShortLived(t, "revoked")

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonRefreshTokenRevoked)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonRefreshTokenRevoked)

		secret := e.tokenSecret(t, object.Name)
		for _, key := range []string{
			controller.KeyAccessToken, controller.KeyAccessTokenExpiresAt, controller.KeyRefreshToken, controller.KeyRefreshTokenExpires,
		} {
			assert.NotContains(t, secret.Data, key)
		}

		require.Eventually(t, func() bool {
			for _, token := range e.replicaTokens(t, object) {
				if token != "" {
					return false
				}
			}

			return true
		}, 30*time.Second, 50*time.Millisecond, "a replica still serves the refused pair")
		assert.GreaterOrEqual(t, e.refreshCount(t, object.Name, "revoked"), 1.0)
	})

	t.Run("a GitHub outage during a refresh keeps the authorization Ready while the access token is valid, and the refresh is retried", func(t *testing.T) {
		var attempts atomic.Int64

		var down atomic.Bool
		down.Store(true)

		e.onRefresh(t, func(writer http.ResponseWriter, _ *http.Request) bool {
			if !down.Load() {
				return false
			}

			attempts.Add(1)
			writer.WriteHeader(http.StatusBadGateway)

			return true
		})

		object, first := e.authorizeShortLived(t, "outage")

		require.Eventually(t, func() bool {
			condition := readyCondition(t, e, object)

			return attempts.Load() >= 2 && condition != nil && condition.Status == metav1.ConditionTrue &&
				strings.Contains(condition.Message, "RefreshFailed")
		}, 30*time.Second, 50*time.Millisecond, "waiting for the failed refreshes")

		assert.Equal(t, controller.ReasonAuthorized,
			apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionAuthorizationRequired).Reason)
		assert.Contains(t, e.replicaTokens(t, object), first)
		assert.GreaterOrEqual(t, e.refreshCount(t, object.Name, "failed"), 2.0)

		e.fake.SetUserTokenLifetimes(0, 0)
		down.Store(false)

		require.Eventually(t, func() bool {
			condition := readyCondition(t, e, object)

			return condition != nil && condition.Status == metav1.ConditionTrue && !strings.Contains(condition.Message, "RefreshFailed") &&
				string(e.secretData(t, object.Name)[controller.KeyAccessToken]) != first
		}, 30*time.Second, 50*time.Millisecond, "waiting for the refresh after the outage")
	})

	t.Run("a refused access token makes the leader refresh at once, and a burst of requests causes one refresh", func(t *testing.T) {
		object, first := e.authorizeWithLifetime(t, "woken", 0)
		name := client.ObjectKeyFromObject(object)

		// The token has 8 hours left, so nothing is due.
		assert.Never(t, func() bool { return e.refreshCount(t, object.Name, "ok") > 0 }, time.Second, 100*time.Millisecond)

		requester := controller.NewRefreshRequester(e.client)
		for range 5 {
			require.NoError(t, requester.Request(t.Context(), name))
		}

		var current string

		require.Eventually(t, func() bool {
			current = string(e.secretData(t, object.Name)[controller.KeyAccessToken])

			return current != first
		}, 30*time.Second, 50*time.Millisecond, "waiting for the woken refresh")

		require.Eventually(t, func() bool {
			for _, token := range e.replicaTokens(t, object) {
				if token != current {
					return false
				}
			}

			return true
		}, 30*time.Second, 50*time.Millisecond, "waiting for every replica to serve the new token")

		assert.Never(t, func() bool { return e.refreshCount(t, object.Name, "ok") > 1 }, 2*time.Second, 100*time.Millisecond)
		assert.Equal(t, 1.0, e.refreshCount(t, object.Name, "ok"))
	})
}
