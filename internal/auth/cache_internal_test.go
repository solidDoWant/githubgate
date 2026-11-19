package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTokenReviewCacheNeverHoldsTheToken(t *testing.T) {
	const token = "secret-projected-token-value"

	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{"githubgate"},
			User:          authenticationv1.UserInfo{Username: "system:serviceaccount:ci-a:builder"},
		}

		return true, review, nil
	})
	authenticator, err := NewTokenReviewAuthenticator(client.AuthenticationV1().TokenReviews(), "githubgate")
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, "/v1/token", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	_, err = authenticator.Authenticate(t.Context(), request)
	require.NoError(t, err)

	require.Equal(t, 1, authenticator.cache.Len())

	for _, key := range authenticator.cache.Keys() {
		assert.NotContains(t, string(key[:]), token)
		assert.NotContains(t, fmt.Sprintf("%x", key), fmt.Sprintf("%x", token))
		value, ok := authenticator.cache.Peek(key)
		require.True(t, ok)
		assert.False(t, strings.Contains(fmt.Sprintf("%#v", value), token))
	}
}
