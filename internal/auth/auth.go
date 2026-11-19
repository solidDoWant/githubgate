// Package auth authenticates callers of githubgate.
//
// An Authenticator turns an HTTP request into the caller's Identity. The
// ServiceAccount token authenticator (TokenReviewAuthenticator) is the only
// implementation; another method can be added behind the same interface.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Identity is the authenticated caller.
type Identity struct {
	// Namespace is the caller's namespace. Policies of this namespace apply.
	Namespace string
	// ServiceAccount is the caller's ServiceAccount name in Namespace.
	ServiceAccount string
	// Pod is the name of the caller's pod when the credential reports it.
	// It is for the audit log only and never used for a decision.
	Pod string
}

// Authenticator authenticates the caller of an HTTP request.
//
// Errors wrap exactly one of ErrNoCredential, ErrInvalidCredential or
// ErrUnavailable; use errors.Is to tell them apart. Error messages never
// contain the credential.
type Authenticator interface {
	Authenticate(ctx context.Context, request *http.Request) (Identity, error)
}

var (
	// ErrNoCredential means the request carries no credential at all.
	ErrNoCredential = errors.New("no credential")
	// ErrInvalidCredential means the credential is malformed, or the
	// authority said definitely that it does not identify a caller.
	ErrInvalidCredential = errors.New("invalid credential")
	// ErrUnavailable means the credential could not be checked. The caller
	// is not authenticated, and the request may succeed later.
	ErrUnavailable = errors.New("authentication unavailable")
)
