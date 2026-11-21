package controller

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

func TestClearSpecChanged(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, githubgatev1alpha1.AddToScheme(scheme))

	object := &githubgatev1alpha1.GitHubUserAuthorization{
		ObjectMeta: metav1.ObjectMeta{Namespace: "githubgate", Name: "user", UID: "uid-1"},
		Spec:       githubgatev1alpha1.GitHubUserAuthorizationSpec{Login: "someone-else"},
	}
	authorization := &model.UserAuthorization{
		NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "user"},
		App:            &model.App{NamespacedName: types.NamespacedName{Namespace: "githubgate", Name: "app"}},
		ClientID:       "Iv1",
		Login:          "someone-else",
		Resolved:       true,
	}

	stored := userTokens{
		accessToken: "ghu_old", accessTokenExpiresAt: time.Now().Add(8 * time.Hour),
		refreshToken: "ghr_old", refreshTokenExpiresAt: time.Now().Add(24 * time.Hour),
		login: "octocat", app: "githubgate/app", clientID: "Iv1",
	}
	newer := stored
	newer.accessToken, newer.refreshToken, newer.login = "ghu_newer", "ghr_newer", "someone-else"

	tokenSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "githubgate", Name: UserTokenSecretName("user"),
				Labels: map[string]string{LabelUserToken: "true"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: githubgatev1alpha1.GroupVersion.String(), Kind: "GitHubUserAuthorization", Name: "user", UID: "uid-1",
				}},
			},
			Data: stored.data(),
		}
	}

	tests := []struct {
		name string
		// between, when set, writes to the Secret after the clear read it
		// and before the clear is stored.
		between     func(*corev1.Secret)
		wantKept    []string
		wantRemoved []string
		// wantRefreshToken is the refresh token left, "" for none.
		wantRefreshToken string
	}{
		{
			name:        "the token keys are removed and what the pair was bound to is kept",
			wantKept:    []string{KeyLogin, KeyApp, KeyClientID},
			wantRemoved: tokenKeys,
		},
		{
			name: "a newer pair written before the clear is stored is kept",
			between: func(secret *corev1.Secret) {
				secret.Data = newer.data()
			},
			wantKept:         append([]string{KeyLogin, KeyApp, KeyClientID}, tokenKeys...),
			wantRefreshToken: "ghr_newer",
		},
		{
			name: "a write that keeps the old pair does not stop the clear",
			between: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"example.com/touched": "true"}
			},
			wantKept:    []string{KeyLogin, KeyApp, KeyClientID},
			wantRemoved: tokenKeys,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			written := false
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tokenSecret()).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, inner client.WithWatch, updated client.Object, options ...client.UpdateOption) error {
						if test.between != nil && !written {
							written = true

							var current corev1.Secret
							require.NoError(t, inner.Get(ctx, client.ObjectKeyFromObject(updated), &current))
							test.between(&current)
							require.NoError(t, inner.Update(ctx, &current))
						}

						return inner.Update(ctx, updated, options...)
					},
				}).Build()

			reconciler := &userAuthorizationReconciler{base: base{client: kube}, workers: newWorkers(), apiReader: kube}
			require.NoError(t, reconciler.clearSpecChanged(t.Context(), object, authorization))

			var secret corev1.Secret
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(tokenSecret()), &secret))

			for _, key := range test.wantRemoved {
				assert.NotContains(t, secret.Data, key)
			}

			for _, key := range test.wantKept {
				assert.Contains(t, secret.Data, key)
			}

			assert.Equal(t, test.wantRefreshToken, string(secret.Data[KeyRefreshToken]))

			if test.between != nil {
				assert.True(t, written, "the clear never tried to store")
			}
		})
	}
}
