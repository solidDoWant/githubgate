//go:build integration

package controller_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/controller"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

const deviceClientID = "Iv23liDeviceFlow"

// startDeviceFlowEnv starts the environment with a Ready App that has a
// client ID and the device flow on.
func startDeviceFlowEnv(t *testing.T) *env {
	t.Helper()

	e := startEnv(t)

	e.fake.CreateOAuthApp(fakegithub.OAuthApp{ClientID: deviceClientID, DeviceFlow: true})
	// Above the client's one-second polling floor, so the fake never answers
	// slow_down.
	e.fake.SetDeviceInterval(500 * time.Millisecond)

	e.keySecret(t, "key", map[string][]byte{"private-key.pem": keyPEM(e.key)})

	object := app("device", "key")
	object.Spec.ClientID = deviceClientID
	e.create(t, object)
	e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionTrue, "Ready")

	return e
}

func deviceAuthorization(name string) *githubgatev1alpha1.GitHubUserAuthorization {
	object := userAuthorization(name, "device")
	object.Annotations = map[string]string{controller.AnnotationAuthorize: "1"}

	return object
}

// requirePending waits for the device flow of object to show its user code,
// and returns it.
func (e *env) requirePending(t *testing.T, object *githubgatev1alpha1.GitHubUserAuthorization) *githubgatev1alpha1.DeviceAuthorizationStatus {
	t.Helper()

	require.Eventually(t, func() bool {
		if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			return false
		}

		return object.Status.Authorization != nil
	}, 30*time.Second, 100*time.Millisecond, "waiting for the user code")

	return object.Status.Authorization
}

func requireAuthorizationRequired(t *testing.T, object *githubgatev1alpha1.GitHubUserAuthorization, status metav1.ConditionStatus, reason string) {
	t.Helper()

	condition := apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionAuthorizationRequired)
	require.NotNil(t, condition)
	assert.Equal(t, status, condition.Status)
	assert.Equal(t, reason, condition.Reason)
}

func (e *env) requireNoTokenSecret(t *testing.T, name string) {
	t.Helper()

	var secret corev1.Secret

	err := e.client.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: controller.UserTokenSecretName(name)}, &secret)
	require.True(t, apierrors.IsNotFound(err), "the token Secret exists: %v", err)
}

// requireNoNewFlow checks that object's status shows no device flow for a
// while.
func (e *env) requireNoNewFlow(t *testing.T, object *githubgatev1alpha1.GitHubUserAuthorization) {
	t.Helper()

	assert.Never(t, func() bool {
		var current githubgatev1alpha1.GitHubUserAuthorization
		if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), &current); err != nil {
			return false
		}

		return current.Status.Authorization != nil
	}, 3*time.Second, 100*time.Millisecond, "a device flow started")
}

// setAuthorizeAnnotation sets the authorize annotation on a fresh copy, since
// the controller may write the object in between.
func (e *env) setAuthorizeAnnotation(t *testing.T, object *githubgatev1alpha1.GitHubUserAuthorization, value string) {
	t.Helper()

	require.NoError(t, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			return err
		}

		object.Annotations = map[string]string{controller.AnnotationAuthorize: value}

		return e.client.Update(t.Context(), object)
	}))
}

func TestDeviceFlowAuthorization(t *testing.T) {
	e := startDeviceFlowEnv(t)

	t.Run("the user code is shown, and once the named user approves it the token is stored and the authorization is Ready", func(t *testing.T) {
		object := deviceAuthorization("approved")
		e.create(t, object)

		pending := e.requirePending(t, object)
		assert.NotEmpty(t, pending.UserCode)
		assert.True(t, strings.HasSuffix(pending.VerificationURI, "/login/device"), pending.VerificationURI)
		assert.Equal(t, "1", pending.Request)
		assert.Equal(t, "1", object.Status.LastAuthorizeRequest)
		assert.WithinDuration(t, time.Now().Add(15*time.Minute), pending.ExpiresAt.Time, time.Minute)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonAuthorizationPending)

		// The login is compared without case.
		require.NoError(t, e.fake.ApproveDeviceCode(pending.UserCode, "OctoCat"))

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionTrue, "Ready")
		requireAuthorizationRequired(t, object, metav1.ConditionFalse, controller.ReasonAuthorized)
		assert.Nil(t, object.Status.Authorization)
		assert.Equal(t, "OctoCat", object.Status.Login)
		require.NotNil(t, object.Status.AccessTokenExpiresAt)
		require.NotNil(t, object.Status.RefreshTokenExpiresAt)
		assert.WithinDuration(t, time.Now().Add(8*time.Hour), object.Status.AccessTokenExpiresAt.Time, time.Minute)
		assert.WithinDuration(t, time.Now().Add(180*24*time.Hour), object.Status.RefreshTokenExpiresAt.Time, time.Hour)

		var secret corev1.Secret
		require.NoError(t, e.client.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: "approved-user-token"}, &secret))
		assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
		assert.Equal(t, "true", secret.Labels["githubgate.soliddowant.dev/user-token"])
		require.Len(t, secret.OwnerReferences, 1)
		assert.Equal(t, object.UID, secret.OwnerReferences[0].UID)
		assert.Equal(t, "GitHubUserAuthorization", secret.OwnerReferences[0].Kind)
		require.NotNil(t, secret.OwnerReferences[0].BlockOwnerDeletion)
		assert.False(t, *secret.OwnerReferences[0].BlockOwnerDeletion)

		var accessToken, refreshToken string

		for _, issued := range e.fake.UserTokens() {
			switch issued.Kind {
			case fakegithub.AccessToken:
				accessToken = issued.Token
			case fakegithub.RefreshToken:
				refreshToken = issued.Token
			}
		}

		assert.Equal(t, accessToken, string(secret.Data["access-token"]))
		assert.Equal(t, refreshToken, string(secret.Data["refresh-token"]))
		assert.Equal(t, "OctoCat", string(secret.Data["login"]))
		assert.Equal(t, adminNamespace+"/device", string(secret.Data["app"]))
		assert.Equal(t, deviceClientID, string(secret.Data["client-id"]))
		assert.Equal(t, object.Status.AccessTokenExpiresAt.UTC().Format(time.RFC3339), string(secret.Data["access-token-expires-at"]))
		assert.Equal(t, object.Status.RefreshTokenExpiresAt.UTC().Format(time.RFC3339), string(secret.Data["refresh-token-expires-at"]))

		// The value was handled, so nothing starts another flow.
		e.requireNoNewFlow(t, object)
	})

	t.Run("a code approved by another GitHub user stores nothing and reports WrongUser", func(t *testing.T) {
		object := deviceAuthorization("wrong-user")
		e.create(t, object)

		pending := e.requirePending(t, object)
		require.NoError(t, e.fake.ApproveDeviceCode(pending.UserCode, "someone-else"))

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonWrongUser)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonWrongUser)
		assert.Nil(t, object.Status.Authorization)
		assert.Empty(t, object.Status.Login)
		e.requireNoTokenSecret(t, object.Name)
		e.requireNoNewFlow(t, object)
	})

	t.Run("a re-authorization approved by another user keeps the stored token in use and keeps reporting WrongUser", func(t *testing.T) {
		object, _ := e.authorizeWithLifetime(t, "reauthwrong", 0)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionTrue, "Ready")

		e.setAuthorizeAnnotation(t, object, "2")
		require.Eventually(t, func() bool {
			if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
				return false
			}

			return object.Status.Authorization != nil && object.Status.Authorization.Request == "2"
		}, 30*time.Second, 100*time.Millisecond)
		require.NoError(t, e.fake.ApproveDeviceCode(object.Status.Authorization.UserCode, "someone-else"))

		kept := func() bool {
			if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
				return false
			}

			ready := apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionReady)
			required := apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionAuthorizationRequired)

			return object.Status.Authorization == nil && ready != nil && required != nil &&
				ready.Status == metav1.ConditionTrue && ready.Reason == "Ready" &&
				required.Status == metav1.ConditionFalse && required.Reason == controller.ReasonWrongUser &&
				strings.Contains(ready.Message, "still in use") && strings.Contains(ready.Message, "request 2")
		}
		require.Eventually(t, kept, 30*time.Second, 100*time.Millisecond)

		assert.Never(t, func() bool { return !kept() }, 3*time.Second, 100*time.Millisecond, "the failed attempt was overwritten")
		assert.NotNil(t, e.secretData(t, object.Name))
	})

	t.Run("a denied code stores nothing and reports AuthorizationDenied", func(t *testing.T) {
		object := deviceAuthorization("denied")
		e.create(t, object)

		pending := e.requirePending(t, object)
		require.NoError(t, e.fake.DenyDeviceCode(pending.UserCode))

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationDenied)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonAuthorizationDenied)
		e.requireNoTokenSecret(t, object.Name)
		e.requireNoNewFlow(t, object)
	})

	t.Run("an App with the device flow off reports DeviceFlowDisabled and stores nothing", func(t *testing.T) {
		require.NoError(t, e.fake.SetDeviceFlow(deviceClientID, false))
		t.Cleanup(func() { require.NoError(t, e.fake.SetDeviceFlow(deviceClientID, true)) })

		object := deviceAuthorization("disabled")
		e.create(t, object)

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonDeviceFlowDisabled)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonDeviceFlowDisabled)
		assert.Equal(t, "1", object.Status.LastAuthorizeRequest)
		e.requireNoTokenSecret(t, object.Name)

		// Turning the device flow on starts nothing until the annotation changes.
		require.NoError(t, e.fake.SetDeviceFlow(deviceClientID, true))
		e.requireNoNewFlow(t, object)

		e.setAuthorizeAnnotation(t, object, "2")
		assert.Equal(t, "2", e.requirePending(t, object).Request)
	})

	t.Run("no flow starts without an annotation", func(t *testing.T) {
		object := userAuthorization("unannotated", "device")
		e.create(t, object)

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationRequired)
		e.requireNoNewFlow(t, object)
	})

	t.Run("a flow the leader does not hold is reported lost and is not started again", func(t *testing.T) {
		object := userAuthorization("lost", "device")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationRequired)

		// What a leader that stopped during a flow leaves behind. The
		// controller may write the status in between, so each write is
		// retried on a fresh copy.
		require.NoError(t, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			if err := e.client.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
				return err
			}

			object.Status.LastAuthorizeRequest = "7"
			object.Status.Authorization = &githubgatev1alpha1.DeviceAuthorizationStatus{
				UserCode: "BCDF-GHJK", VerificationURI: "https://github.com/login/device",
				ExpiresAt: metav1.NewTime(time.Now().Add(10 * time.Minute)), Request: "7",
			}

			return e.client.Status().Update(t.Context(), object)
		}))

		e.setAuthorizeAnnotation(t, object, "7")

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonFlowLost)
		requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonFlowLost)
		assert.Nil(t, object.Status.Authorization)
		e.requireNoNewFlow(t, object)
	})

	t.Run("an existing Secret of the token Secret's name that githubgate did not create is never adopted", func(t *testing.T) {
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "foreign-user-token"},
			Data:       map[string][]byte{"access-token": []byte("not ours")},
		}
		e.create(t, foreign)

		object := deviceAuthorization("foreign")
		e.create(t, object)

		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "SecretInvalid")
		e.requireNoNewFlow(t, object)

		var secret corev1.Secret
		require.NoError(t, e.client.Get(t.Context(), client.ObjectKeyFromObject(foreign), &secret))
		assert.Equal(t, map[string][]byte{"access-token": []byte("not ours")}, secret.Data)
		assert.Empty(t, secret.OwnerReferences)
		assert.Empty(t, secret.Labels)
	})

	// Runs last, so it searches what every flow above wrote.
	t.Run("no user token, refresh token or device code appears in status, events or metrics", func(t *testing.T) {
		var authorizations githubgatev1alpha1.GitHubUserAuthorizationList
		require.NoError(t, e.client.List(t.Context(), &authorizations))

		var events corev1.EventList
		require.NoError(t, e.client.List(t.Context(), &events))

		corpus, err := json.Marshal([]any{authorizations, events})
		require.NoError(t, err)

		for _, gatherer := range e.gatherers {
			families, err := gatherer.Gather()
			require.NoError(t, err)

			for _, family := range families {
				corpus = fmt.Append(corpus, family.String())
			}
		}

		// The search is only meaningful if it reads what the flows wrote.
		require.Contains(t, string(corpus), "OctoCat", "the status is collected")

		secrets := e.fake.DeviceCodes()
		require.NotEmpty(t, secrets, "the flows used device codes")

		for _, issued := range e.fake.UserTokens() {
			secrets = append(secrets, issued.Token)
		}

		for _, secret := range secrets {
			require.NotEmpty(t, secret)
			assert.NotContains(t, string(corpus), secret, "a secret of %d characters leaked", len(secret))
		}
	})
}

func TestDeviceFlowExpiredCode(t *testing.T) {
	e := startDeviceFlowEnv(t)

	object := deviceAuthorization("expired")
	e.create(t, object)
	e.requirePending(t, object)

	// Past the user code's 15 minutes on the fake's clock.
	e.fake.AdvanceClock(16 * time.Minute)

	e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationExpired)
	requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonAuthorizationExpired)
	assert.Nil(t, object.Status.Authorization)
	e.requireNoTokenSecret(t, object.Name)
	e.requireNoNewFlow(t, object)
}
