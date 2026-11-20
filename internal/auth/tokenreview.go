package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultCacheTTL is how long a definite review result is reused. It
	// bounds how long a deleted ServiceAccount's token keeps working.
	DefaultCacheTTL = 30 * time.Second
	// DefaultCacheSize is the most review results kept at once.
	DefaultCacheSize = 10000

	// podNameExtraKey is the user extra the API server sets to the name of
	// the pod a projected token is bound to.
	podNameExtraKey = "authentication.kubernetes.io/pod-name"
	// serviceAccountUsernamePrefix starts every ServiceAccount's username:
	// system:serviceaccount:<namespace>:<name>.
	serviceAccountUsernamePrefix = "system:serviceaccount:"
)

// TokenReviewCreator creates TokenReviews. The client-go TokenReview client
// (clientset.AuthenticationV1().TokenReviews()) satisfies it.
type TokenReviewCreator interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, options metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// Option configures a TokenReviewAuthenticator.
type Option func(*TokenReviewAuthenticator)

// WithCacheTTL sets how long a definite review result is reused.
func WithCacheTTL(ttl time.Duration) Option {
	return func(authenticator *TokenReviewAuthenticator) { authenticator.cacheTTL = ttl }
}

// reviewResult is a definite review result: an identity, or a refusal.
type reviewResult struct {
	identity      Identity
	authenticated bool
}

// TokenReviewAuthenticator authenticates callers by a Kubernetes
// ServiceAccount token, checked with the TokenReview API for one audience.
type TokenReviewAuthenticator struct {
	reviews  TokenReviewCreator
	audience string
	cacheTTL time.Duration
	// cache maps the SHA-256 of a token to its definite review result, so
	// the token itself is never kept.
	cache *expirable.LRU[[sha256.Size]byte, reviewResult]
}

var _ Authenticator = (*TokenReviewAuthenticator)(nil)

// NewTokenReviewAuthenticator returns an authenticator that reviews tokens
// with reviews, requiring audience in the review's audiences.
func NewTokenReviewAuthenticator(reviews TokenReviewCreator, audience string, options ...Option) (*TokenReviewAuthenticator, error) {
	if reviews == nil {
		return nil, errors.New("a TokenReview client is required")
	}

	if audience == "" {
		return nil, errors.New("an audience is required")
	}

	authenticator := &TokenReviewAuthenticator{reviews: reviews, audience: audience, cacheTTL: DefaultCacheTTL}
	for _, option := range options {
		option(authenticator)
	}

	if authenticator.cacheTTL <= 0 || authenticator.cacheTTL > DefaultCacheTTL {
		return nil, fmt.Errorf("the cache TTL must be more than 0 and at most %s", DefaultCacheTTL)
	}

	authenticator.cache = expirable.NewLRU[[sha256.Size]byte, reviewResult](DefaultCacheSize, nil, authenticator.cacheTTL)

	return authenticator, nil
}

// Authenticate implements Authenticator.
func (a *TokenReviewAuthenticator) Authenticate(ctx context.Context, request *http.Request) (Identity, error) {
	token, err := bearerToken(request)
	if err != nil {
		return Identity{}, err
	}

	key := sha256.Sum256([]byte(token))
	if result, ok := a.cache.Get(key); ok {
		return result.outcome()
	}

	result, err := a.review(ctx, token)
	if err != nil {
		return Identity{}, err
	}

	a.cache.Add(key, result)

	return result.outcome()
}

func (r reviewResult) outcome() (Identity, error) {
	if !r.authenticated {
		return Identity{}, fmt.Errorf("%w: the ServiceAccount token was not accepted", ErrInvalidCredential)
	}

	return r.identity, nil
}

// review asks the API server about token. It returns an error only when the
// API server gave no definite answer.
func (a *TokenReviewAuthenticator) review(ctx context.Context, token string) (reviewResult, error) {
	review, err := a.reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		// The client's error never contains the request body, so the token
		// can't leak through it; wrap only its text for the logs.
		return reviewResult{}, fmt.Errorf("%w: TokenReview failed: %s", ErrUnavailable, err.Error())
	}

	if review == nil {
		return reviewResult{}, fmt.Errorf("%w: TokenReview returned no result", ErrUnavailable)
	}

	status := review.Status
	if !status.Authenticated || !slices.Contains(status.Audiences, a.audience) {
		return reviewResult{}, nil
	}

	namespace, name, ok := splitServiceAccountUsername(status.User.Username)
	if !ok {
		return reviewResult{}, nil
	}

	identity := Identity{Namespace: namespace, ServiceAccount: name}
	if pods := status.User.Extra[podNameExtraKey]; len(pods) == 1 {
		identity.Pod = pods[0]
	}

	return reviewResult{identity: identity, authenticated: true}, nil
}

// bearerToken returns the token of the request's one Authorization header:
// "Bearer <token>", "token <token>" (schemes compared case-insensitively) or
// HTTP Basic with the token as the password.
func bearerToken(request *http.Request) (string, error) {
	values := request.Header.Values("Authorization")
	switch len(values) {
	case 0:
		return "", ErrNoCredential
	case 1:
	default:
		return "", fmt.Errorf("%w: more than one Authorization header", ErrInvalidCredential)
	}

	scheme, credential, _ := strings.Cut(values[0], " ")
	switch {
	case strings.EqualFold(scheme, "Bearer"), strings.EqualFold(scheme, "token"):
		token := strings.TrimSpace(credential)
		if token == "" || strings.ContainsAny(token, " \t") {
			return "", fmt.Errorf("%w: the Authorization header has no single token", ErrInvalidCredential)
		}

		return token, nil
	case strings.EqualFold(scheme, "Basic"):
		_, password, ok := request.BasicAuth()
		if !ok || password == "" {
			return "", fmt.Errorf("%w: the Basic credential has no password", ErrInvalidCredential)
		}

		return password, nil
	default:
		return "", fmt.Errorf("%w: unsupported Authorization scheme", ErrInvalidCredential)
	}
}

// splitServiceAccountUsername returns the namespace and name of a
// ServiceAccount username, or false for any other user.
func splitServiceAccountUsername(username string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(username, serviceAccountUsernamePrefix)
	if !found {
		return "", "", false
	}

	namespace, name, found = strings.Cut(rest, ":")
	if !found ||
		len(validation.IsDNS1123Label(namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(name)) != 0 {
		return "", "", false
	}

	return namespace, name, true
}
