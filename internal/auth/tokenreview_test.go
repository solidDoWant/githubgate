package auth_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/githubgate/internal/auth"
)

const (
	testAudience = "githubgate"
	validToken   = "valid-projected-token"
)

// fakeAPIServer answers TokenReview requests like the Kubernetes API server
// does for the tokens it knows, and records every token it is asked about.
type fakeAPIServer struct {
	mu sync.Mutex
	// tokens maps a token to the review status the API server reports for it.
	tokens map[string]authenticationv1.TokenReviewStatus
	// failure, when set, is returned instead of a review.
	failure error
	// reviews counts the TokenReview calls per token.
	reviews map[string]int
	// audiences records the audiences of the last review request.
	audiences []string
}

func newFakeAPIServer() *fakeAPIServer {
	return &fakeAPIServer{
		tokens: map[string]authenticationv1.TokenReviewStatus{
			validToken: serviceAccountStatus("ci-a", "builder", testAudience),
		},
		reviews: map[string]int{},
	}
}

func serviceAccountStatus(namespace, name string, audiences ...string) authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     audiences,
		User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:" + namespace + ":" + name,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"builder-pod"},
			},
		},
	}
}

func (f *fakeAPIServer) set(token string, status authenticationv1.TokenReviewStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tokens[token] = status
}

func (f *fakeAPIServer) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failure = err
}

func (f *fakeAPIServer) reviewCount(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reviews[token]
}

func (f *fakeAPIServer) totalReviews() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	total := 0
	for _, count := range f.reviews {
		total += count
	}

	return total
}

func (f *fakeAPIServer) lastAudiences() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.audiences
}

func (f *fakeAPIServer) react(action k8stesting.Action) (bool, runtime.Object, error) {
	review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.reviews[review.Spec.Token]++

	f.audiences = review.Spec.Audiences
	if f.failure != nil {
		return true, nil, f.failure
	}

	result := review.DeepCopy()

	status, known := f.tokens[review.Spec.Token]
	if !known {
		status = authenticationv1.TokenReviewStatus{Error: "invalid bearer token"}
	}

	result.Status = status

	return true, result, nil
}

func (f *fakeAPIServer) authenticator(t *testing.T, options ...auth.Option) auth.Authenticator {
	t.Helper()

	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", f.react)
	authenticator, err := auth.NewTokenReviewAuthenticator(client.AuthenticationV1().TokenReviews(), testAudience, options...)
	require.NoError(t, err)

	return authenticator
}

func requestWithAuthorization(values ...string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/token", nil)
	for _, value := range values {
		request.Header.Add("Authorization", value)
	}

	return request
}

func basic(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func TestTokenReviewAuthenticatorAcceptsEveryCredentialForm(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
	}{
		{name: "bearer", authorization: "Bearer " + validToken},
		{name: "bearer lower case", authorization: "bearer " + validToken},
		{name: "token", authorization: "token " + validToken},
		{name: "token upper case", authorization: "TOKEN " + validToken},
		{name: "basic password", authorization: basic("x-access-token", validToken)},
		{name: "basic password with empty username", authorization: basic("", validToken)},
		{name: "basic lower case", authorization: "basic " + base64.StdEncoding.EncodeToString([]byte("git:"+validToken))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiServer := newFakeAPIServer()
			authenticator := apiServer.authenticator(t)

			identity, err := authenticator.Authenticate(t.Context(), requestWithAuthorization(test.authorization))
			require.NoError(t, err)
			assert.Equal(t, auth.Identity{Namespace: "ci-a", ServiceAccount: "builder", Pod: "builder-pod"}, identity)
			assert.Equal(t, 1, apiServer.reviewCount(validToken))
			assert.Equal(t, []string{testAudience}, apiServer.lastAudiences())
		})
	}
}

func TestTokenReviewAuthenticatorWithoutPodName(t *testing.T) {
	apiServer := newFakeAPIServer()
	status := serviceAccountStatus("ci-b", "deployer", testAudience)
	status.User.Extra = nil
	apiServer.set("no-pod-token", status)
	authenticator := apiServer.authenticator(t)

	identity, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer no-pod-token"))
	require.NoError(t, err)
	assert.Equal(t, auth.Identity{Namespace: "ci-b", ServiceAccount: "deployer"}, identity)
}

func TestTokenReviewAuthenticatorWithoutCredential(t *testing.T) {
	apiServer := newFakeAPIServer()
	authenticator := apiServer.authenticator(t)

	_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization())
	require.ErrorIs(t, err, auth.ErrNoCredential)
}

func TestTokenReviewAuthenticatorRejectsMalformedCredentials(t *testing.T) {
	tests := []struct {
		name           string
		authorizations []string
	}{
		{name: "bearer and basic at once", authorizations: []string{"Bearer " + validToken, basic("x", validToken)}},
		{name: "two bearer headers", authorizations: []string{"Bearer " + validToken, "Bearer " + validToken}},
		{name: "empty header", authorizations: []string{""}},
		{name: "empty bearer", authorizations: []string{"Bearer "}},
		{name: "bearer without value", authorizations: []string{"Bearer"}},
		{name: "blank bearer", authorizations: []string{"Bearer    "}},
		{name: "empty token", authorizations: []string{"token "}},
		{name: "empty basic password", authorizations: []string{basic("x-access-token", "")}},
		{name: "basic without password", authorizations: []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(validToken))}},
		{name: "basic not base64", authorizations: []string{"Basic %%%"}},
		{name: "another scheme", authorizations: []string{"Digest " + validToken}},
		{name: "scheme only token", authorizations: []string{validToken}},
		{name: "two values in one bearer header", authorizations: []string{"Bearer " + validToken + " " + validToken}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiServer := newFakeAPIServer()
			authenticator := apiServer.authenticator(t)

			_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization(test.authorizations...))
			require.ErrorIs(t, err, auth.ErrInvalidCredential)
			assert.Zero(t, apiServer.totalReviews(), "a malformed credential is never sent for review")
		})
	}
}

func TestTokenReviewAuthenticatorRejectsTokensTheAPIServerRefuses(t *testing.T) {
	tests := []struct {
		name   string
		status authenticationv1.TokenReviewStatus
	}{
		{
			name:   "expired token",
			status: authenticationv1.TokenReviewStatus{Authenticated: false, Error: "token has expired"},
		},
		{
			name:   "wrong audience",
			status: authenticationv1.TokenReviewStatus{Authenticated: false, Error: "token audiences [\"other\"] is invalid for the target audiences [\"githubgate\"]"},
		},
		{
			name: "not authenticated though the audience matches",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: false,
				Audiences:     []string{testAudience},
				User:          serviceAccountStatus("ci-a", "builder").User,
			},
		},
		{
			name:   "authenticated for another audience only",
			status: serviceAccountStatus("ci-a", "builder", "https://kubernetes.default.svc"),
		},
		{
			name:   "authenticated without audiences",
			status: serviceAccountStatus("ci-a", "builder"),
		},
		{
			name: "user that is not a ServiceAccount",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "alice"},
			},
		},
		{
			name: "system user shaped like a namespace and name",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "system:anonymous"},
			},
		},
		{
			name: "node user",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "system:node:worker-1"},
			},
		},
		{
			name: "ServiceAccount username without a name",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "system:serviceaccount:ci-a:"},
			},
		},
		{
			name: "ServiceAccount username without a namespace",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "system:serviceaccount::builder"},
			},
		},
		{
			name: "ServiceAccount username with an extra part",
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{testAudience},
				User:          authenticationv1.UserInfo{Username: "system:serviceaccount:ci-a:builder:extra"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiServer := newFakeAPIServer()
			apiServer.set("refused-token", test.status)
			authenticator := apiServer.authenticator(t)

			_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer refused-token"))
			require.ErrorIs(t, err, auth.ErrInvalidCredential)
			assert.NotContains(t, err.Error(), "refused-token", "errors never carry the token")
		})
	}
}

func TestTokenReviewAuthenticatorReportsAPIFailureAsUnavailableAndDoesNotCacheIt(t *testing.T) {
	apiServer := newFakeAPIServer()
	apiServer.setFailure(errors.New("connection refused"))
	authenticator := apiServer.authenticator(t)

	_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer "+validToken))
	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.NotErrorIs(t, err, auth.ErrInvalidCredential)
	assert.NotContains(t, err.Error(), validToken, "errors never carry the token")

	apiServer.setFailure(nil)

	identity, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer "+validToken))
	require.NoError(t, err)
	assert.Equal(t, "builder", identity.ServiceAccount)
	assert.Equal(t, 2, apiServer.reviewCount(validToken), "the failure was not cached")
}

func TestTokenReviewAuthenticatorCachesDefiniteResults(t *testing.T) {
	apiServer := newFakeAPIServer()
	authenticator := apiServer.authenticator(t)

	for range 3 {
		_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer "+validToken))
		require.NoError(t, err)
		_, err = authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer unknown-token"))
		require.ErrorIs(t, err, auth.ErrInvalidCredential)
	}

	assert.Equal(t, 1, apiServer.reviewCount(validToken))
	assert.Equal(t, 1, apiServer.reviewCount("unknown-token"))
}

func TestTokenReviewAuthenticatorRefusesDeletedServiceAccountAfterCacheExpiry(t *testing.T) {
	apiServer := newFakeAPIServer()
	authenticator := apiServer.authenticator(t, auth.WithCacheTTL(50*time.Millisecond))

	_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer "+validToken))
	require.NoError(t, err)

	// Deleting the ServiceAccount makes the API server refuse its tokens.
	apiServer.set(validToken, authenticationv1.TokenReviewStatus{Error: "service account deleted"})

	require.Eventually(t, func() bool {
		_, err := authenticator.Authenticate(t.Context(), requestWithAuthorization("Bearer "+validToken))
		return errors.Is(err, auth.ErrInvalidCredential)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestDefaultCacheTTLIsAtMostThirtySeconds(t *testing.T) {
	assert.LessOrEqual(t, auth.DefaultCacheTTL, 30*time.Second)
	assert.Positive(t, auth.DefaultCacheTTL)
}

func TestNewTokenReviewAuthenticatorRequiresAnAudience(t *testing.T) {
	client := fake.NewClientset()
	_, err := auth.NewTokenReviewAuthenticator(client.AuthenticationV1().TokenReviews(), "")
	require.Error(t, err)
}
