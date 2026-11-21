package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/model"
)

const (
	// LabelUserToken marks the Secrets githubgate keeps user tokens in. It
	// writes no Secret without it.
	LabelUserToken = "githubgate.soliddowant.dev/user-token"

	// The keys of a user-token Secret.
	KeyAccessToken          = "access-token"
	KeyAccessTokenExpiresAt = "access-token-expires-at"
	KeyRefreshToken         = "refresh-token"
	KeyRefreshTokenExpires  = "refresh-token-expires-at"
	KeyLogin                = "login"
	KeyApp                  = "app"
	KeyClientID             = "client-id"

	userTokenSuffix = "-user-token"
)

// UserTokenSecretName is the name of the Secret holding an authorization's
// user tokens.
func UserTokenSecretName(authorization string) string {
	return authorization + userTokenSuffix
}

// errForeignSecret means a Secret of the token Secret's name exists that
// githubgate didn't create; it is never adopted.
var errForeignSecret = errors.New("a Secret of that name exists that githubgate did not create")

// userTokens is a token pair with what it is bound to.
type userTokens struct {
	accessToken           string
	accessTokenExpiresAt  time.Time
	refreshToken          string
	refreshTokenExpiresAt time.Time
	login                 string
	app                   string
	clientID              string
}

func (t userTokens) data() map[string][]byte {
	return map[string][]byte{
		KeyAccessToken:          []byte(t.accessToken),
		KeyAccessTokenExpiresAt: []byte(t.accessTokenExpiresAt.UTC().Format(time.RFC3339)),
		KeyRefreshToken:         []byte(t.refreshToken),
		KeyRefreshTokenExpires:  []byte(t.refreshTokenExpiresAt.UTC().Format(time.RFC3339)),
		KeyLogin:                []byte(t.login),
		KeyApp:                  []byte(t.app),
		KeyClientID:             []byte(t.clientID),
	}
}

// ownsUserToken reports whether secret is a token Secret githubgate created
// for object: it carries the label and object's owner reference.
func ownsUserToken(secret *corev1.Secret, object *githubgatev1alpha1.GitHubUserAuthorization) bool {
	if secret.Labels[LabelUserToken] != "true" {
		return false
	}

	for _, owner := range secret.OwnerReferences {
		if owner.UID == object.UID && owner.Kind == "GitHubUserAuthorization" && owner.Name == object.Name {
			return true
		}
	}

	return false
}

// storeUserTokens writes tokens to object's token Secret. It creates the
// Secret, or updates it on the resourceVersion just read when githubgate owns
// it; it never touches a Secret it didn't create.
func storeUserTokens(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	tokens userTokens,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: object.Namespace,
			Name:      UserTokenSecretName(object.Name),
			Labels:    map[string]string{LabelUserToken: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: githubgatev1alpha1.GroupVersion.String(),
				Kind:       "GitHubUserAuthorization",
				Name:       object.Name,
				UID:        object.UID,
				// No finalizers permission is needed; garbage collection
				// still deletes the Secret with its authorization.
				BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: tokens.data(),
	}

	err := writer.Create(ctx, secret)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}

	var current corev1.Secret
	if err := reader.Get(ctx, client.ObjectKeyFromObject(secret), &current); err != nil {
		return err
	}

	if !ownsUserToken(&current, object) {
		return errForeignSecret
	}

	current.Data = tokens.data()

	// The update carries current's resourceVersion, so it fails on any write
	// in between instead of overwriting it.
	return writer.Update(ctx, &current)
}

// tokenState is what an authorization's token Secret holds.
type tokenState int

const (
	tokenMissing tokenState = iota
	tokenForeign
	tokenSpecChanged
	tokenValid
)

// readUserTokens reads object's token Secret through reader and says
// whether its token may be used for authorization.
func readUserTokens(
	ctx context.Context,
	reader client.Reader,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	authorization *model.UserAuthorization,
) (tokenState, userTokens, error) {
	_, state, tokens, err := getUserTokens(ctx, reader, object, authorization)

	return state, tokens, err
}

// getUserTokens is readUserTokens that also returns the Secret read, for a
// write conditional on its resourceVersion.
func getUserTokens(
	ctx context.Context,
	reader client.Reader,
	object *githubgatev1alpha1.GitHubUserAuthorization,
	authorization *model.UserAuthorization,
) (*corev1.Secret, tokenState, userTokens, error) {
	var secret corev1.Secret

	err := reader.Get(ctx, types.NamespacedName{Namespace: object.Namespace, Name: UserTokenSecretName(object.Name)}, &secret)
	if apierrors.IsNotFound(err) {
		return nil, tokenMissing, userTokens{}, nil
	}

	if err != nil {
		return nil, tokenMissing, userTokens{}, err
	}

	if !ownsUserToken(&secret, object) {
		return &secret, tokenForeign, userTokens{}, nil
	}

	tokens := userTokens{
		accessToken:  string(secret.Data[KeyAccessToken]),
		refreshToken: string(secret.Data[KeyRefreshToken]),
		login:        string(secret.Data[KeyLogin]),
		app:          string(secret.Data[KeyApp]),
		clientID:     string(secret.Data[KeyClientID]),
	}
	tokens.accessTokenExpiresAt, _ = time.Parse(time.RFC3339, string(secret.Data[KeyAccessTokenExpiresAt]))
	tokens.refreshTokenExpiresAt, _ = time.Parse(time.RFC3339, string(secret.Data[KeyRefreshTokenExpires]))

	if tokens.accessToken == "" && tokens.refreshToken == "" {
		return &secret, tokenMissing, userTokens{}, nil
	}

	if !strings.EqualFold(tokens.login, authorization.Login) || tokens.app != authorization.App.String() ||
		tokens.clientID != authorization.ClientID {
		return &secret, tokenSpecChanged, tokens, nil
	}

	return &secret, tokenValid, tokens, nil
}
