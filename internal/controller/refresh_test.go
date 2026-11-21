package controller_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/controller"
	"github.com/solidDoWant/githubgate/internal/model"
)

func TestUserAccessToken(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, githubgatev1alpha1.AddToScheme(scheme))

	object := &githubgatev1alpha1.GitHubUserAuthorization{
		ObjectMeta: metav1.ObjectMeta{Namespace: "githubgate", Name: "user", UID: "uid-1"},
		Spec:       githubgatev1alpha1.GitHubUserAuthorizationSpec{Login: "octocat"},
	}
	authorization := &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "user"},
		App:            &model.App{NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "app"}},
		ClientID:       "Iv1",
		Login:          "octocat",
		Resolved:       true,
	}

	secret := func(expiresIn time.Duration, change func(*corev1.Secret)) *corev1.Secret {
		result := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "githubgate", Name: controller.UserTokenSecretName("user"),
				Labels: map[string]string{controller.LabelUserToken: "true"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: githubgatev1alpha1.GroupVersion.String(), Kind: "GitHubUserAuthorization", Name: "user", UID: "uid-1",
				}},
			},
			Data: map[string][]byte{
				controller.KeyAccessToken:          []byte("ghu_stored"),
				controller.KeyAccessTokenExpiresAt: []byte(time.Now().Add(expiresIn).UTC().Format(time.RFC3339)),
				controller.KeyRefreshToken:         []byte("ghr_stored"),
				controller.KeyRefreshTokenExpires:  []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)),
				controller.KeyLogin:                []byte("OctoCat"),
				controller.KeyApp:                  []byte("githubgate/app"),
				controller.KeyClientID:             []byte("Iv1"),
			},
		}
		if change != nil {
			change(result)
		}

		return result
	}

	tests := []struct {
		name   string
		secret *corev1.Secret
		want   string
	}{
		{name: "a token with more than five minutes left is served", secret: secret(6*time.Minute, nil), want: "ghu_stored"},
		{name: "a token within five minutes of its expiry counts as missing", secret: secret(4*time.Minute, nil)},
		{name: "an expired token counts as missing", secret: secret(-time.Minute, nil)},
		{name: "no Secret means no token"},
		{name: "a Secret without the label is not used", secret: secret(time.Hour, func(s *corev1.Secret) { s.Labels = nil })},
		{name: "a Secret of another owner is not used", secret: secret(time.Hour, func(s *corev1.Secret) { s.OwnerReferences[0].UID = "uid-2" })},
		{name: "a token of another login is not used", secret: secret(time.Hour, func(s *corev1.Secret) { s.Data[controller.KeyLogin] = []byte("hubot") })},
		{name: "a token of another client ID is not used", secret: secret(time.Hour, func(s *corev1.Secret) { s.Data[controller.KeyClientID] = []byte("Iv2") })},
		{name: "cleared token keys mean no token", secret: secret(time.Hour, func(s *corev1.Secret) {
			delete(s.Data, controller.KeyAccessToken)
			delete(s.Data, controller.KeyRefreshToken)
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []client.Object{object.DeepCopy()}
			if test.secret != nil {
				objects = append(objects, test.secret)
			}

			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			token, ok, err := controller.UserAccessToken(t.Context(), reader, authorization)
			require.NoError(t, err)
			assert.Equal(t, test.want, token)
			assert.Equal(t, test.want != "", ok)
		})
	}
}
