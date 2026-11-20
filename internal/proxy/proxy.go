// Package proxy serves githubgate's proxy mode: it accepts requests meant
// for GitHub's API, uploads and git hosts, authenticates the caller with its
// ServiceAccount token, refuses requests it can't classify safely, and
// classifies the rest before they are decided and forwarded.
package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/solidDoWant/githubgate/internal/auth"
)

// The error codes of proxy refusals.
const (
	codeInvalidRequest       = "invalid_request"
	codeUnauthenticated      = "unauthenticated"
	codeUnknownOperation     = "unknown_operation"
	codeNotFound             = "not_found"
	codeTooLarge             = "too_large"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeMisdirected          = "misdirected_request"
	codeNotReady             = "not_ready"
	codeUnavailable          = "unavailable"
)

// documentationURL is the user page refusals link to.
const documentationURL = "https://github.com/solidDoWant/githubgate/blob/master/docs/user/getting-access.md"

// Forwarder decides and forwards a classified request for an authenticated
// caller. The request's Authorization header is already removed.
type Forwarder interface {
	Forward(writer http.ResponseWriter, request *http.Request, caller auth.Identity, classified Request)
}

// Options configure a Handler.
type Options struct {
	Authenticator auth.Authenticator
	// Ready reports whether the replica can decide requests; always ready
	// when nil.
	Ready func() bool
	// Forwarder receives every classified request. When nil, classified
	// requests are refused with 503, so nothing is ever forwarded.
	Forwarder Forwarder
	// Audit receives one line for each request the handler refuses itself;
	// the Forwarder writes the line of the others. Discarded when nil.
	Audit *slog.Logger
	// Count, when set, is called once for each request the handler refuses
	// itself, with the status the caller got.
	Count func(decision string, status int)
}

// Handler serves the proxy.
type Handler struct {
	options Options
}

// NewHandler returns the proxy's handler.
func NewHandler(options Options) *Handler {
	if options.Audit == nil {
		options.Audit = slog.New(slog.DiscardHandler)
	}

	return &Handler{options: options}
}

// ServeHTTP checks the Host, authenticates the caller and classifies the
// request before handing it to the Forwarder.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()

	var (
		host   string
		caller auth.Identity
	)

	writeRefusal := func(writer http.ResponseWriter, host string, graphQL bool, failure *refusal) {
		writeRefusal(writer, host, graphQL, failure)
		auditRefusal(request.Context(), h.options.Audit, started, caller, host, request, failure)

		if h.options.Count != nil {
			h.options.Count("refused", failure.status)
		}
	}

	host, err := canonicalHost(request.Host)
	if err != nil {
		writeRefusal(writer, "", false, refuse(http.StatusMisdirectedRequest, codeMisdirected,
			"the Host header must name api.github.com, uploads.github.com or github.com"))

		return
	}

	graphQL := host == HostAPI && request.Method == http.MethodPost && request.URL.Path == "/graphql"

	if h.options.Ready != nil && !h.options.Ready() {
		writeRefusal(writer, host, graphQL, refuse(http.StatusServiceUnavailable, codeNotReady, "githubgate is not ready; try again"))

		return
	}

	caller, err = h.options.Authenticator.Authenticate(request.Context(), request)

	switch {
	case errors.Is(err, auth.ErrUnavailable):
		writeRefusal(writer, host, graphQL, refuse(http.StatusServiceUnavailable, codeUnavailable,
			"the caller's credential can't be checked now; try again"))

		return
	case err != nil:
		writer.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		writeRefusal(writer, host, graphQL, refuse(http.StatusUnauthorized, codeUnauthenticated,
			"a valid ServiceAccount token is required"))

		return
	}

	classified, failure := classify(host, request)
	if failure != nil {
		writeRefusal(writer, host, graphQL, failure)

		return
	}

	request.Header.Del("Authorization")

	if h.options.Forwarder == nil {
		writeRefusal(writer, host, graphQL, refuse(http.StatusServiceUnavailable, codeUnavailable, "requests can't be forwarded"))

		return
	}

	h.options.Forwarder.Forward(writer, request, caller, classified)
}

// errorBody is the JSON body of a refusal on the API hosts.
type errorBody struct {
	Message          string         `json:"message"`
	Error            string         `json:"error"`
	DocumentationURL string         `json:"documentation_url"`
	Errors           []graphQLError `json:"errors,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

// writeRefusal answers a refusal as the host's clients expect: JSON on the
// API hosts (with GraphQL's errors list for GraphQL), text on github.com and
// on a host the proxy doesn't serve.
func writeRefusal(writer http.ResponseWriter, host string, graphQL bool, failure *refusal) {
	message := "githubgate: " + failure.message

	writer.Header().Set("Cache-Control", "no-store")

	if host != HostAPI && host != HostUploads {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.WriteHeader(failure.status)
		_, _ = writer.Write([]byte(message + "\n"))

		return
	}

	body := errorBody{Message: message, Error: failure.code, DocumentationURL: documentationURL}
	if graphQL {
		body.Errors = []graphQLError{{Message: message}}
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(failure.status)
	_ = json.NewEncoder(writer).Encode(body)
}
