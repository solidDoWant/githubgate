// Package server serves githubgate's token API and its health and metrics
// endpoints, and writes one audit line per token request.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/resolve"
)

// maxBodyBytes is the largest request body the token API reads.
const maxBodyBytes = 64 << 10

// The error reasons the token API answers with besides resolution outcomes.
const (
	reasonUnauthenticated = "unauthenticated"
	reasonNotReady        = "not_ready"
	reasonUnavailable     = "unavailable"
	reasonGitHubUnavail   = string(github.ReasonUnavailable)
	reasonInvalidRequest  = string(resolve.OutcomeInvalidRequest)
	reasonDenied          = string(resolve.OutcomeDenied)
)

// Options configure a Server.
type Options struct {
	Authenticator auth.Authenticator
	Store         *model.Store
	GitHub        *github.Registry
	// Metrics receives the request metrics; nothing is recorded when nil.
	Metrics *Metrics
	// Audit receives one line per token request; discarded when nil.
	Audit *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Server answers token requests. It is safe for concurrent use.
type Server struct {
	options  Options
	tokens   *tokenCache
	draining atomic.Bool
}

// New returns a Server.
func New(options Options) *Server {
	if options.Now == nil {
		options.Now = time.Now
	}

	if options.Audit == nil {
		options.Audit = slog.New(slog.DiscardHandler)
	}

	return &Server{options: options, tokens: newTokenCache(options.Now)}
}

// TokenRequest is the body of POST /v1/token. Every field is optional.
type TokenRequest struct {
	Installation string                       `json:"installation,omitempty"`
	Owner        string                       `json:"owner,omitempty"`
	Repositories []string                     `json:"repositories,omitempty"`
	Permissions  map[string]permissions.Level `json:"permissions,omitempty"`
}

// TokenResponse is the answer to an allowed token request. Repositories and
// permissions are GitHub's values.
type TokenResponse struct {
	Token        string                       `json:"token"`
	ExpiresAt    time.Time                    `json:"expiresAt"`
	Repositories []string                     `json:"repositories"`
	Permissions  map[string]permissions.Level `json:"permissions"`
}

// ErrorResponse is the body of every refusal.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Ready reports whether the replica can serve: its snapshot is built and it
// isn't shutting down.
func (s *Server) Ready() bool { return !s.draining.Load() && s.options.Store.Ready() }

// Handler returns the token API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", s.serveToken)

	return mux
}

// answer is what a token request ended with, for the response, audit and metrics.
type answer struct {
	status     int
	reason     string
	message    string
	retryAfter time.Duration
	token      *github.Token
	github     string
	caller     auth.Identity
	decision   resolve.Decision
	requested  TokenRequest
}

func (s *Server) serveToken(writer http.ResponseWriter, request *http.Request) {
	started := s.options.Now()
	result := s.token(writer, request)

	s.respond(writer, result)
	s.audit(request, started, result)

	if metrics := s.options.Metrics; metrics != nil {
		decision := "refused"
		if result.status == http.StatusOK {
			decision = "allowed"
		}

		metrics.requests.WithLabelValues("token", decision, codeClass(result.status)).Inc()
	}
}

func (s *Server) token(writer http.ResponseWriter, request *http.Request) answer {
	// Only a missing snapshot refuses: while shutting down, requests are
	// still served so that callers routed here before /readyz failed succeed.
	if !s.options.Store.Ready() {
		return refusal(http.StatusServiceUnavailable, reasonNotReady, "githubgate is not ready; try again")
	}

	caller, err := s.options.Authenticator.Authenticate(request.Context(), request)
	s.countAuthentication(err)

	switch {
	case errors.Is(err, auth.ErrUnavailable):
		return refusal(http.StatusServiceUnavailable, reasonUnavailable, "the caller's credential can't be checked now; try again")
	case err != nil:
		writer.Header().Set("WWW-Authenticate", "Bearer")

		return refusal(http.StatusUnauthorized, reasonUnauthenticated, "a valid ServiceAccount token is required: "+authProblem(err))
	}

	var body TokenRequest

	request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		result := refusal(http.StatusBadRequest, reasonInvalidRequest, "the request body is not a valid token request: "+bodyProblem(err))
		result.caller = caller

		return result
	}

	snapshot := s.options.Store.Load()
	decision := resolve.Resolve(
		resolve.Identity{Namespace: caller.Namespace, ServiceAccount: caller.ServiceAccount},
		resolve.Request{Installation: body.Installation, Owner: body.Owner, Repositories: body.Repositories, Permissions: body.Permissions},
		snapshot,
	)

	result := answer{caller: caller, decision: decision, requested: body, github: "not_called"}

	if !decision.Allowed() {
		result.status, result.reason, result.message = decision.Outcome.Status(), string(decision.Outcome), decision.Message

		return result
	}

	// Resolution refuses an empty set already; this keeps GitHub from ever
	// being asked for a token without limits.
	if len(decision.Repositories) == 0 || len(decision.Permissions) == 0 {
		result.status, result.reason = http.StatusForbidden, reasonDenied
		result.message = "the request resolved to no repository or no permission"

		return result
	}

	return s.mint(request, result)
}

func (s *Server) mint(request *http.Request, result answer) answer {
	installation := result.decision.Installation
	app := installation.App

	client, err := s.options.GitHub.Client(app.NamespacedName, github.Credentials{
		AppID: app.AppID, PrivateKey: app.PrivateKey(), Version: app.KeyVersion,
	})
	if err != nil {
		return s.githubFailure(result, err)
	}

	installationID, err := client.InstallationID(request.Context(), installation.Owner)
	if err != nil {
		return s.githubFailure(result, err)
	}

	key := newTokenKey(result.caller.Namespace, result.caller.ServiceAccount, app.AppID, installationID,
		result.decision.Repositories, result.decision.Permissions)

	token, hit := s.tokens.get(key)
	s.countCache(hit)

	if !hit {
		token, err = s.tokens.mint(request.Context(), key, func(ctx context.Context) (*github.Token, error) {
			started := time.Now()
			defer s.observeGitHub("mint", started)

			return client.Mint(ctx, github.MintRequest{
				Installation: installation.String(),
				Owner:        installation.Owner,
				Repositories: result.decision.Repositories,
				Permissions:  result.decision.Permissions,
			})
		})
		if err != nil {
			return s.githubFailure(result, err)
		}

		result.github = "minted"
	} else {
		result.github = "cached"
	}

	result.status, result.reason, result.token = http.StatusOK, string(resolve.OutcomeAllowed), token

	return result
}

func (s *Server) githubFailure(result answer, err error) answer {
	var failure *github.Error

	switch {
	case errors.As(err, &failure):
		result.status, result.reason, result.message = failure.HTTPStatus(), string(failure.Reason), failure.Message
		result.retryAfter = failure.RetryAfter
		result.github = string(failure.Reason)
	case errors.Is(err, github.ErrEmptyScope):
		result.status, result.reason, result.message = http.StatusForbidden, reasonDenied, "the request resolved to no repository or no permission"
	default:
		// Other errors carry no credential, but their text isn't meant for
		// callers: a fixed message is enough.
		result.status, result.reason, result.message = http.StatusServiceUnavailable, reasonGitHubUnavail, "GitHub could not be used"
		result.github = reasonGitHubUnavail
	}

	return result
}

func (s *Server) respond(writer http.ResponseWriter, result answer) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")

	if result.retryAfter > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(int((result.retryAfter+time.Second-1)/time.Second)))
	}

	writer.WriteHeader(result.status)

	if result.status == http.StatusOK {
		_ = json.NewEncoder(writer).Encode(TokenResponse{
			Token:        result.token.Token,
			ExpiresAt:    result.token.ExpiresAt,
			Repositories: result.token.Repositories,
			Permissions:  result.token.Permissions,
		})

		return
	}

	_ = json.NewEncoder(writer).Encode(ErrorResponse{Error: result.reason, Message: result.message})
}

// audit writes the request's audit line. It never contains a token or a
// header value.
func (s *Server) audit(request *http.Request, started time.Time, result answer) {
	decision := "refused"
	if result.status == http.StatusOK {
		decision = "allowed"
	}

	repositories := result.requested.Repositories
	permissionSet := result.requested.Permissions
	installation := result.requested.Installation

	if result.decision.Allowed() {
		repositories, permissionSet = result.decision.Repositories, result.decision.Permissions
	}

	if result.decision.Installation != nil {
		installation = result.decision.Installation.String()
	}

	rules := make([]string, 0, len(result.decision.Rules))
	for _, rule := range result.decision.Rules {
		rules = append(rules, rule.Policy.String()+"/"+rule.Name)
	}

	s.options.Audit.LogAttrs(request.Context(), slog.LevelInfo, "token request",
		slog.String("request_id", requestID()),
		slog.String("namespace", result.caller.Namespace),
		slog.String("service_account", result.caller.ServiceAccount),
		slog.String("pod", result.caller.Pod),
		slog.String("installation", installation),
		slog.String("mode", "token"),
		slog.Any("repositories", nonNil(repositories)),
		slog.String("permissions", formatPermissions(permissionSet)),
		slog.Any("rules", rules),
		slog.String("decision", decision),
		slog.String("reason", result.reason),
		slog.Int("status", result.status),
		slog.String("github_status", result.github),
		slog.Duration("duration", s.options.Now().Sub(started)),
	)
}

func (s *Server) countAuthentication(err error) {
	if s.options.Metrics == nil {
		return
	}

	result := "authenticated"

	switch {
	case errors.Is(err, auth.ErrNoCredential):
		result = "no_credential"
	case errors.Is(err, auth.ErrUnavailable):
		result = "unavailable"
	case err != nil:
		result = "invalid"
	}

	s.options.Metrics.tokenReviews.WithLabelValues(result).Inc()
}

func (s *Server) countCache(hit bool) {
	if s.options.Metrics == nil {
		return
	}

	result := "miss"
	if hit {
		result = "hit"
	}

	s.options.Metrics.tokenCache.WithLabelValues(result).Inc()
}

func (s *Server) observeGitHub(operation string, started time.Time) {
	if s.options.Metrics != nil {
		s.options.Metrics.githubDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds())
	}
}

func refusal(status int, reason, message string) answer {
	return answer{status: status, reason: reason, message: message, github: "not_called"}
}

// authProblem names what was wrong with the credential without repeating it.
func authProblem(err error) string {
	if errors.Is(err, auth.ErrNoCredential) {
		return "the request has none"
	}

	return "the one sent is not valid for githubgate"
}

// bodyProblem describes a body error without echoing the body.
func bodyProblem(err error) string {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Sprintf("it is larger than %d bytes", maxBodyBytes)
	}

	if strings.Contains(err.Error(), "unknown field") {
		return "it has a field the token API doesn't know"
	}

	return "it is not a JSON object of the documented fields"
}

func formatPermissions(set map[string]permissions.Level) string {
	parts := make([]string, 0, len(set))
	for name, level := range set {
		parts = append(parts, name+":"+string(level))
	}

	sort.Strings(parts)

	return strings.Join(parts, ",")
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

func requestID() string {
	var raw [8]byte

	_, _ = rand.Read(raw[:])

	return hex.EncodeToString(raw[:])
}
