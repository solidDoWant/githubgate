package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/operations"
	"github.com/solidDoWant/githubgate/internal/proxy/graphql"
)

const (
	// codeGitHubUnavailable answers an upstream that can't be reached, times
	// out, or refuses the token githubgate minted.
	codeGitHubUnavailable = "github_unavailable"
	// maxRefBodyBytes is the largest body read to find the refs it writes.
	maxRefBodyBytes = 1 << 20
	// upstreamDialTimeout and upstreamHeaderTimeout bound connecting to
	// GitHub and waiting for its response headers.
	upstreamDialTimeout   = 10 * time.Second
	upstreamHeaderTimeout = 60 * time.Second
)

// TokenSource mints, or reuses, the installation token an allowed decision
// gives the caller.
type TokenSource interface {
	// ProxyToken returns the token and how GitHub was used ("minted",
	// "cached", or a failure reason). A refusal is a *TokenError.
	ProxyToken(ctx context.Context, caller auth.Identity, decision Decision) (token, githubStatus string, err error)
}

// UserTokens gives the current access token of a user authorization. It
// never refreshes: the leader does.
type UserTokens interface {
	// UserToken returns the access token, or "" and the reason none is
	// usable now. Neither the reason nor the error contains a credential.
	UserToken(ctx context.Context, authorization *model.UserAuthorization) (token, reason string, err error)
}

// TokenError is a token that couldn't be had, as the caller sees it. Message
// never contains a credential.
type TokenError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *TokenError) Error() string { return e.Code + ": " + e.Message }

// Upstreams are the servers each GitHub host's requests are sent to. They
// come from configuration, never from the request.
type Upstreams struct {
	API, Uploads, Git *url.URL
}

// DefaultUpstreams are GitHub's own hosts.
func DefaultUpstreams() Upstreams {
	return Upstreams{
		API:     &url.URL{Scheme: "https", Host: HostAPI},
		Uploads: &url.URL{Scheme: "https", Host: HostUploads},
		Git:     &url.URL{Scheme: "https", Host: HostGit},
	}
}

func (u Upstreams) of(host string) *url.URL {
	switch host {
	case HostAPI:
		return u.API
	case HostUploads:
		return u.Uploads
	case HostGit:
		return u.Git
	default:
		return nil
	}
}

// NewTransport returns the transport to GitHub: the system roots plus
// extraRoots when set, and the dial and response header timeouts.
func NewTransport(extraRoots []byte) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = upstreamHeaderTimeout
	transport.Proxy = nil

	if len(extraRoots) > 0 {
		pool, err := systemPoolWith(extraRoots)
		if err != nil {
			return nil, err
		}

		transport.TLSClientConfig.RootCAs = pool
	}

	return transport, nil
}

// ForwarderOptions configure NewForwarder.
type ForwarderOptions struct {
	// Rules returns a caller's accepted rules from the current snapshot;
	// each request is decided against one call.
	Rules  func(namespace, serviceAccount string) []*model.Rule
	Tokens TokenSource
	// UserTokens gives user rules their access token; with none, every
	// request a user rule allows is refused with 503.
	UserTokens UserTokens
	Upstreams  Upstreams
	// Transport sends requests upstream; NewTransport(nil) when nil.
	Transport http.RoundTripper
	// Audit receives one line per forwarded or refused request; discarded
	// when nil.
	Audit *slog.Logger
	// Count, when set, is called once per request with its decision
	// ("allowed" or "refused") and the status the caller got.
	Count func(decision string, status int)
}

// HTTPForwarder decides classified requests and forwards the allowed ones
// to GitHub with an installation token in place of the caller's credential.
type HTTPForwarder struct {
	options ForwarderOptions
}

// NewForwarder returns a forwarder.
func NewForwarder(options ForwarderOptions) (*HTTPForwarder, error) {
	if options.Transport == nil {
		transport, err := NewTransport(nil)
		if err != nil {
			return nil, err
		}

		options.Transport = transport
	}

	if options.Audit == nil {
		options.Audit = slog.New(slog.DiscardHandler)
	}

	return &HTTPForwarder{options: options}, nil
}

// Forward decides the request and forwards it when allowed. It writes the
// request's audit line when the response is finished.
func (f *HTTPForwarder) Forward(writer http.ResponseWriter, request *http.Request, caller auth.Identity, classified Request) {
	entry := &auditEntry{started: time.Now(), caller: caller, host: classified.Host, method: request.Method, githubStatus: "not_called"}
	recorder := &responseRecorder{ResponseWriter: writer}

	defer func() {
		entry.status, entry.bytes = recorder.statusOrOK(), recorder.bytes
		entry.write(request.Context(), f.options.Audit)

		if f.options.Count != nil {
			f.options.Count(entry.decision(), entry.status)
		}
	}()

	graphQL := classified.Kind == KindGraphQL

	decided, failure := readBody(request, classified)
	entry.decided = decided

	if failure != nil {
		entry.reason = failure.code
		writeRefusal(recorder, classified.Host, graphQL, failure)

		return
	}

	rules := f.options.Rules(caller.Namespace, caller.ServiceAccount)
	decision := Decide(rules, decided)
	entry.decisionResult = &decision

	if !decision.Allowed() {
		entry.reason = decision.Code

		if decided.push != nil {
			writePushRefusal(recorder, request.Body, decided.push, decision.Message, refusedRefs(rules, decided))

			return
		}

		writeRefusal(recorder, classified.Host, graphQL, refuse(decision.Status, decision.Code, decision.Message))

		return
	}

	if decision.Access == AccessWeb {
		entry.reason = "allowed"
		f.reverseProxy(classified, "", entry).ServeHTTP(recorder, request)

		return
	}

	if decision.Access == AccessUser {
		f.forwardAsUser(recorder, request, classified, decision, entry)

		return
	}

	token, githubStatus, err := f.options.Tokens.ProxyToken(request.Context(), caller, decision)
	entry.githubStatus = githubStatus

	if err != nil {
		var tokenError *TokenError
		if !errors.As(err, &tokenError) {
			tokenError = &TokenError{Status: http.StatusServiceUnavailable, Code: codeGitHubUnavailable, Message: "GitHub could not be used"}
		}

		if tokenError.RetryAfter > 0 {
			recorder.Header().Set("Retry-After", strconv.Itoa(int((tokenError.RetryAfter+time.Second-1)/time.Second)))
		}

		entry.reason = tokenError.Code
		writeRefusal(recorder, classified.Host, graphQL, refuse(tokenError.Status, tokenError.Code, tokenError.Message))

		return
	}

	entry.reason = "allowed"
	f.reverseProxy(classified, token, entry).ServeHTTP(recorder, request)
}

// forwardAsUser forwards a request a user rule allows with its
// authorization's current access token, or refuses it with 503 when there
// is none. It never falls back to an installation token.
func (f *HTTPForwarder) forwardAsUser(recorder http.ResponseWriter, request *http.Request, classified Request, decision Decision, entry *auditEntry) {
	authorization := decision.UserAuthorization
	token, reason := "", "no user token source is configured"

	if f.options.UserTokens != nil {
		var err error

		token, reason, err = f.options.UserTokens.UserToken(request.Context(), authorization)
		if err != nil {
			token, reason = "", "its stored token can't be read now"
		}
	}

	if token == "" {
		refusal := UserNotReady(authorization.NamespacedName, reason)
		entry.reason = refusal.Code
		writeRefusal(recorder, classified.Host, false, refuse(refusal.Status, refusal.Code, refusal.Message))

		return
	}

	entry.reason = "allowed"
	f.reverseProxy(classified, token, entry).ServeHTTP(recorder, request)
}

// reverseProxy returns the proxy for one request: the upstream comes from
// the host table, the path and query from the checked request, and the
// credential is the installation token, or none when token is empty.
func (f *HTTPForwarder) reverseProxy(classified Request, token string, entry *auditEntry) *httputil.ReverseProxy {
	upstream := f.options.Upstreams.of(classified.Host)

	return &httputil.ReverseProxy{
		Transport:     f.options.Transport,
		FlushInterval: -1,
		Rewrite: func(out *httputil.ProxyRequest) {
			path, err := url.PathUnescape(classified.Path)
			if err != nil {
				// classify accepted the path, so it decodes; keep the
				// escaped form rather than send something else.
				path = classified.Path
			}

			base := strings.TrimSuffix(upstream.Path, "/")
			out.Out.URL = &url.URL{
				Scheme:   upstream.Scheme,
				Host:     upstream.Host,
				Path:     base + path,
				RawPath:  base + classified.Path,
				RawQuery: classified.RawQuery,
			}
			out.Out.Host = upstream.Host

			for name := range out.Out.Header {
				if strings.HasPrefix(name, "X-Forwarded-") {
					out.Out.Header.Del(name)
				}
			}

			out.Out.Header.Del("Cookie")
			out.Out.Header.Del("Authorization")
			out.Out.Header.Del("Proxy-Authorization")

			switch {
			case token == "":
				// A web path is sent with no credential at all.
			case classified.Host == HostGit:
				out.Out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
			default:
				out.Out.Header.Set("Authorization", "token "+token)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			entry.githubStatus = strconv.Itoa(response.StatusCode)

			// GitHub refusing the token githubgate just minted is not the
			// caller's fault, and is never retried.
			if response.StatusCode == http.StatusUnauthorized && token != "" {
				return errUpstreamRefusedToken
			}

			response.Header.Del("Set-Cookie")

			return nil
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			entry.reason = codeGitHubUnavailable
			if !errors.Is(err, errUpstreamRefusedToken) {
				entry.githubStatus = codeGitHubUnavailable
			}

			if user := entry.userAuthorization(); user != nil && errors.Is(err, errUpstreamRefusedToken) {
				// GitHub refused the stored user token: the authorization
				// can't serve until the leader refreshes it.
				refusal := UserNotReady(user.NamespacedName, "GitHub refused its access token")
				entry.reason = refusal.Code
				writeRefusal(writer, classified.Host, false, refuse(refusal.Status, refusal.Code, refusal.Message))

				return
			}

			writeRefusal(writer, classified.Host, classified.Kind == KindGraphQL,
				refuse(http.StatusServiceUnavailable, codeGitHubUnavailable, "GitHub could not be reached or refused githubgate's token"))
		},
	}
}

var errUpstreamRefusedToken = errors.New("GitHub refused the token githubgate sent")

// readBody reads what the decision needs from the body: a GraphQL
// document's mutations, and the refs a REST operation writes. It replaces
// the body with the bytes to forward.
func readBody(request *http.Request, classified Request) (Decided, *refusal) {
	decided := Decided{Request: classified}

	switch classified.Kind {
	case KindGraphQL:
		parsed, err := graphql.Classify(bodyOf(request))
		if err != nil {
			var classifyError *graphql.Error
			if errors.As(err, &classifyError) {
				return decided, refuse(classifyError.Status, classifyError.Code, classifyError.Reason)
			}

			return decided, refuse(http.StatusBadRequest, codeInvalidRequest, "the GraphQL body can't be read")
		}

		decided.Mutations = parsed.Mutations
		replaceBody(request, parsed.Body)
	case KindGitReceive:
		commands, failure := readReceiveCommands(bodyOf(request))
		if failure != nil {
			return decided, failure
		}

		decided.Refs = commands.Refs
		decided.push = commands

		// The section read is forwarded unchanged, followed by the rest of
		// the stream; the length is unchanged.
		rest := bodyOf(request)
		request.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(commands.Raw), rest), closer: request.Body}
	case KindREST:
		return readRESTRefs(request, decided)
	}

	return decided, nil
}

func readRESTRefs(request *http.Request, decided Decided) (Decided, *refusal) {
	switch operations.RESTRefCheck(decided.Operation) {
	case operations.RefsPathRef:
		match, ok := operations.MatchREST(request.Method, decided.Path)
		ref, hasRef := match.Params["ref"]

		if !ok || !hasRef {
			decided.RefsUnknown = true

			return decided, nil
		}

		decided.Refs = []string{"refs/" + unescaped(ref)}
	case operations.RefsBodyRef, operations.RefsReleaseTag:
		data, err := io.ReadAll(io.LimitReader(bodyOf(request), maxRefBodyBytes+1))
		if err != nil {
			return decided, refuse(http.StatusBadRequest, codeInvalidRequest, "the request body can't be read")
		}

		if len(data) > maxRefBodyBytes {
			return decided, refuse(http.StatusRequestEntityTooLarge, codeTooLarge, "a body larger than 1 MiB is not accepted for this operation")
		}

		replaceBody(request, data)

		value, err := graphql.DecodeJSON(data)
		if err != nil {
			return decided, refuse(http.StatusBadRequest, codeInvalidRequest, "the request body is not valid JSON: "+err.Error())
		}

		object, isObject := value.(map[string]any)
		if !isObject {
			decided.RefsUnknown = true

			return decided, nil
		}

		field, prefix := "ref", ""
		if operations.RESTRefCheck(decided.Operation) == operations.RefsReleaseTag {
			field, prefix = "tag_name", "refs/tags/"
		}

		raw, present := object[field]
		name, isString := raw.(string)

		switch {
		case !present && prefix != "":
			// A release update without tag_name writes no tag, unless it
			// publishes a draft: GitHub then creates the draft's tag. Any
			// draft value other than true may publish it.
			draft, hasDraft := object["draft"]
			decided.PublishesRelease = decided.Operation == "repos/update-release" && hasDraft && draft != true
		case !isString:
			decided.RefsUnknown = true
		default:
			decided.Refs = []string{prefix + name}
		}
	}

	return decided, nil
}

// readCloser reads from Reader and closes closer, which may be nil.
type readCloser struct {
	io.Reader
	closer io.Closer
}

func (r readCloser) Close() error {
	if r.closer == nil {
		return nil
	}

	return r.closer.Close()
}

func bodyOf(request *http.Request) io.Reader {
	if request.Body == nil {
		return http.NoBody
	}

	return request.Body
}

func replaceBody(request *http.Request, data []byte) {
	if request.Body != nil {
		_ = request.Body.Close()
	}

	request.Body = io.NopCloser(bytes.NewReader(data))
	request.ContentLength = int64(len(data))
	request.Header.Del("Transfer-Encoding")
	request.TransferEncoding = nil
	request.Header.Set("Content-Length", strconv.Itoa(len(data)))
}

// responseRecorder counts the bytes and keeps the status of a response.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 && status >= 200 {
		r.status = status
	}

	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}

	written, err := r.ResponseWriter.Write(data)
	r.bytes += int64(written)

	return written, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}

	return r.status
}
