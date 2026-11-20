package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// Reason is the error code a caller of the token API gets for a GitHub failure.
type Reason string

// The reasons a GitHub call can fail with.
const (
	// ReasonRefused means GitHub refused the request: the caller gets 403.
	ReasonRefused Reason = "github_refused"
	// ReasonUnavailable means GitHub could not decide: the caller gets 503.
	ReasonUnavailable Reason = "github_unavailable"
	// ReasonInvalidRequest means the request can never succeed: the caller gets 400.
	ReasonInvalidRequest Reason = "invalid_request"
)

// The status condition reasons a GitHub failure implies for the App or
// installation it concerns.
const (
	ConditionSecretInvalid         = "SecretInvalid"
	ConditionInstallationSuspended = "InstallationSuspended"
	ConditionInstallationNotFound  = "InstallationNotFound"
)

// Error is a failed GitHub call. Its text is built only from fixed strings and
// GitHub's own error message, never from a key, JWT or token.
type Error struct {
	Reason Reason
	// Message is the human text for the caller.
	Message string
	// Condition is the status condition reason this failure implies, if any.
	Condition string
	// RetryAfter is how long GitHub asked the caller to wait, if it did.
	RetryAfter time.Duration
}

func (e *Error) Error() string { return string(e.Reason) + ": " + e.Message }

// HTTPStatus is the status code the token API answers with for this error.
func (e *Error) HTTPStatus() int {
	switch e.Reason {
	case ReasonRefused:
		return http.StatusForbidden
	case ReasonInvalidRequest:
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

// ErrEmptyScope is returned, without calling GitHub, for a mint without
// repositories or permissions: GitHub would widen the token to everything.
var ErrEmptyScope = errors.New("a token needs at least one repository and one permission")

// maxRepositories is GitHub's limit on the repositories of one installation token:
// https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
const maxRepositories = 500

func unavailable(message string) *Error {
	return &Error{Reason: ReasonUnavailable, Message: message}
}

// classify maps an error from a GitHub call to an Error. Only the status code,
// GitHub's message and the rate-limit headers are read from it.
func classify(err error, now time.Time) *Error {
	var rateLimit *gogithub.RateLimitError
	if errors.As(err, &rateLimit) {
		failure := unavailable("GitHub rate limit exceeded")
		if wait := rateLimit.Rate.Reset.Sub(now); wait > 0 {
			failure.RetryAfter = wait
		}

		return failure
	}

	var abuse *gogithub.AbuseRateLimitError
	if errors.As(err, &abuse) {
		failure := unavailable("GitHub secondary rate limit exceeded")
		if abuse.RetryAfter != nil {
			failure.RetryAfter = *abuse.RetryAfter
		}

		return failure
	}

	var response *gogithub.ErrorResponse
	if !errors.As(err, &response) || response.Response == nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return unavailable("GitHub did not answer in time")
		}

		return unavailable("GitHub could not be reached")
	}

	message := response.Message

	switch status := response.Response.StatusCode; {
	case status == http.StatusUnprocessableEntity:
		return &Error{Reason: ReasonRefused, Message: "GitHub refused the token: " + message}
	case status == http.StatusUnauthorized:
		return &Error{Reason: ReasonUnavailable, Message: "GitHub rejected the App's credentials", Condition: ConditionSecretInvalid}
	// GitHub answers a rate limit with 403 or 429 and either Retry-After or
	// X-RateLimit-Remaining: 0:
	// https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit
	case status == http.StatusTooManyRequests,
		status == http.StatusForbidden && (response.Response.Header.Get("Retry-After") != "" || response.Response.Header.Get("X-RateLimit-Remaining") == "0"):
		failure := unavailable("GitHub rate limit exceeded")
		failure.RetryAfter = retryAfter(response.Response.Header)

		return failure
	case status == http.StatusForbidden:
		failure := &Error{Reason: ReasonRefused, Message: "GitHub refused the token: " + message}
		if strings.Contains(strings.ToLower(message), "suspended") {
			failure.Condition = ConditionInstallationSuspended
		}

		return failure
	case status == http.StatusNotFound:
		return &Error{Reason: ReasonUnavailable, Message: "GitHub does not know the installation", Condition: ConditionInstallationNotFound}
	default:
		return unavailable("GitHub answered with status " + strconv.Itoa(status))
	}
}

func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 0
	}

	return time.Duration(seconds) * time.Second
}
