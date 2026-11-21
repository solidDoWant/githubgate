//go:build integration

package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/controller"
)

const (
	createSigningKey = "users/create-ssh-signing-key-for-authenticated-user"
	deleteSigningKey = "users/delete-ssh-signing-key-for-authenticated-user"
)

func userAuthorization(name, appName string) *githubgatev1alpha1.GitHubUserAuthorization {
	return &githubgatev1alpha1.GitHubUserAuthorization{
		ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: name},
		Spec: githubgatev1alpha1.GitHubUserAuthorizationSpec{
			AppRef:            githubgatev1alpha1.ObjectReference{Name: appName},
			Login:             "octocat",
			Ceiling:           githubgatev1alpha1.UserCeiling{Operations: []githubgatev1alpha1.OperationName{createSigningKey}},
			AllowedNamespaces: githubgatev1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
		},
	}
}

func userRule(name, authorization string, operations ...githubgatev1alpha1.OperationPattern) githubgatev1alpha1.AccessRule {
	return githubgatev1alpha1.AccessRule{
		Name:                 name,
		UserAuthorizationRef: &githubgatev1alpha1.ObjectReference{Namespace: adminNamespace, Name: authorization},
		Proxy:                &githubgatev1alpha1.ProxyMode{Enabled: true, AllowOperations: operations},
	}
}

func ruleReasons(t *testing.T, policy *githubgatev1alpha1.GitHubAccessPolicy) map[string]string {
	t.Helper()

	reasons := map[string]string{}

	for _, ruleStatus := range policy.Status.Rules {
		condition := apimeta.FindStatusCondition(ruleStatus.Conditions, controller.ConditionAccepted)
		require.NotNil(t, condition)
		reasons[ruleStatus.Name] = string(condition.Status) + "/" + condition.Reason
	}

	return reasons
}

func TestStatusOfUserAuthorizations(t *testing.T) {
	e := startEnv(t)

	e.keySecret(t, "key", map[string][]byte{"private-key.pem": keyPEM(e.key)})

	inline := app("inline-client", "key")
	inline.Spec.ClientID = "Iv23liAbCdEf012345"
	e.create(t, inline)

	fromSecret := app("secret-client", "key")
	fromSecret.Spec.ClientIDSecretRef = &githubgatev1alpha1.SecretKeyReference{Name: "client-ids"}
	e.create(t, fromSecret)

	noClient := app("no-client", "key")
	e.create(t, noClient)

	t.Run("an authorization whose App has an inline client ID requires authorization", func(t *testing.T) {
		object := userAuthorization("inline", "inline-client")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationRequired)

		required := apimeta.FindStatusCondition(object.Status.Conditions, controller.ConditionAuthorizationRequired)
		require.NotNil(t, required)
		assert.Equal(t, metav1.ConditionTrue, required.Status)
		assert.Equal(t, controller.ReasonNotAuthorized, required.Reason)
		assert.Equal(t, object.Generation, object.Status.ObservedGeneration)
	})

	t.Run("a missing client ID Secret is reported on the authorization and recovers, and the App stays Ready", func(t *testing.T) {
		object := userAuthorization("from-secret", "secret-client")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "SecretUnavailable")

		e.requireReady(t, fromSecret, func() []metav1.Condition { return fromSecret.Status.Conditions }, metav1.ConditionTrue, "Ready")

		e.create(t, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "client-ids"},
			Data:       map[string][]byte{"client-id": []byte("Iv23liFromSecret")},
		})
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonAuthorizationRequired)
	})

	t.Run("an App without a client ID makes the authorization an invalid spec, and the App stays Ready", func(t *testing.T) {
		object := userAuthorization("no-client", "no-client")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "InvalidSpec")
		e.requireReady(t, noClient, func() []metav1.Condition { return noClient.Status.Conditions }, metav1.ConditionTrue, "Ready")
	})

	t.Run("a missing App is reported on the authorization", func(t *testing.T) {
		object := userAuthorization("no-app", "nonexistent")
		e.create(t, object)
		e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, "RefNotFound")
	})

	t.Run("a user rule is accepted only inside the ceiling and from an admitted namespace", func(t *testing.T) {
		require.NoError(t, e.client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "outsider"}}))

		tenant := &githubgatev1alpha1.GitHubAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "user-policy"},
			Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
				ServiceAccounts: []string{"provisioner"},
				Rules: []githubgatev1alpha1.AccessRule{
					userRule("inside", "inline", createSigningKey),
					userRule("outside", "inline", createSigningKey, deleteSigningKey),
				},
			},
		}
		e.create(t, tenant)
		e.requireReady(t, tenant, func() []metav1.Condition { return tenant.Status.Conditions }, metav1.ConditionFalse, controller.ReasonRulesNotAccepted)
		assert.Equal(t, map[string]string{"inside": "True/Accepted", "outside": "False/OutsideCeiling"}, ruleReasons(t, tenant))

		outsider := &githubgatev1alpha1.GitHubAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "outsider", Name: "user-policy"},
			Spec: githubgatev1alpha1.GitHubAccessPolicySpec{
				ServiceAccounts: []string{"provisioner"},
				Rules:           []githubgatev1alpha1.AccessRule{userRule("inside", "inline", createSigningKey)},
			},
		}
		e.create(t, outsider)
		e.requireReady(t, outsider, func() []metav1.Condition { return outsider.Status.Conditions }, metav1.ConditionFalse, controller.ReasonRulesNotAccepted)
		assert.Equal(t, map[string]string{"inside": "False/NotAllowed"}, ruleReasons(t, outsider))
	})
}
