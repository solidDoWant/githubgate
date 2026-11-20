package proxy

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/solidDoWant/githubgate/internal/operations"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

// The three GitHub hosts the proxy serves.
const (
	HostAPI     = "api.github.com"
	HostUploads = "uploads.github.com"
	HostGit     = "github.com"
)

// maxFormBytes is the largest form body read to look for a method override.
const maxFormBytes = 1 << 20

// Kind is what sort of request a classified request is.
type Kind string

// The kinds of request the proxy classifies.
const (
	KindREST        Kind = "rest"
	KindGraphQL     Kind = "graphql"
	KindGitInfoRefs Kind = "git-info-refs"
	KindGitUpload   Kind = "git-upload-pack"
	KindGitReceive  Kind = "git-receive-pack"
)

// Request is a classified proxy request.
type Request struct {
	// Host is the GitHub host the request is for, one of the Host constants.
	Host string
	Kind Kind
	// Operation is the REST operation id; empty for other kinds.
	Operation string
	// Owner and Repo are the repository the request is for, empty when it
	// names none. REST values are as they appeared in the path.
	Owner, Repo string
	// Contents is the contents level a git request needs; empty otherwise
	// (GitHub enforces the permissions of REST and GraphQL requests).
	Contents permissions.Level
	// Path is the checked, still escaped request path.
	Path string
	// RawQuery is the query, parsed and encoded again.
	RawQuery string
}

// refusal is a request the proxy answers itself.
type refusal struct {
	status  int
	code    string
	message string
}

func (r *refusal) Error() string { return r.message }

func refuse(status int, code, message string) *refusal {
	return &refusal{status: status, code: code, message: message}
}

var errNotAllowedHost = errors.New("not a GitHub host the proxy serves")

// canonicalHost returns the GitHub host a Host header names: lower-cased,
// with an optional ":443", no trailing dot, exactly one of the three hosts.
func canonicalHost(header string) (string, error) {
	host := strings.ToLower(header)
	host = strings.TrimSuffix(host, ":443")

	switch host {
	case HostAPI, HostUploads, HostGit:
		return host, nil
	default:
		return "", errNotAllowedHost
	}
}

// methodOverrideHeaders are the headers some servers read as the real method.
var methodOverrideHeaders = []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"}

// classify checks request and says what it is. host is its canonical host.
// It may replace request.Body with the bytes it read.
func classify(host string, request *http.Request) (Request, *refusal) {
	if failure := refuseOutright(request); failure != nil {
		return Request{}, failure
	}

	rawPath, rawQuery, _ := strings.Cut(request.RequestURI, "?")

	if err := checkPath(rawPath); err != nil {
		return Request{}, refuse(http.StatusBadRequest, codeInvalidRequest, "the request path is not accepted: "+err.Error())
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return Request{}, refuse(http.StatusBadRequest, codeInvalidRequest, "the request query can't be parsed")
	}

	if query.Has("_method") {
		return Request{}, refuse(http.StatusBadRequest, codeInvalidRequest, "a method override is not accepted")
	}

	if failure := refuseFormOverride(request); failure != nil {
		return Request{}, failure
	}

	classified, failure := classifyKind(Request{Host: host, Path: rawPath, RawQuery: query.Encode()}, request.Method, query)
	if failure != nil {
		return Request{}, failure
	}

	if failure := refuseGzippedBody(request, classified); failure != nil {
		return Request{}, failure
	}

	return classified, nil
}

// classifyKind says what kind of request a checked request is.
func classifyKind(classified Request, method string, query url.Values) (Request, *refusal) {
	rawPath := classified.Path

	switch classified.Host {
	case HostGit:
		return classifyGit(classified, method, query)
	case HostAPI:
		if method == http.MethodPost && rawPath == "/graphql" {
			classified.Kind = KindGraphQL

			return classified, nil
		}
	}

	match, ok := operations.MatchREST(method, rawPath)
	if !ok {
		return Request{}, refuse(http.StatusForbidden, codeUnknownOperation,
			"the request matches no operation of GitHub's REST API description")
	}

	classified.Kind, classified.Operation = KindREST, match.ID
	classified.Owner, _ = match.Owner()
	classified.Repo, _ = match.Repo()

	return classified, nil
}

// refuseOutright refuses methods, upgrades, method overrides and encodings
// the proxy never forwards. A gzipped body is checked once the request is
// classified.
func refuseOutright(request *http.Request) *refusal {
	switch request.Method {
	case http.MethodConnect, http.MethodTrace, http.MethodOptions:
		return refuse(http.StatusBadRequest, codeInvalidRequest, "the method "+request.Method+" is not accepted")
	}

	if _, upgrade := request.Header["Upgrade"]; upgrade {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "a protocol upgrade is not accepted")
	}

	for _, name := range methodOverrideHeaders {
		if _, present := request.Header[http.CanonicalHeaderKey(name)]; present {
			return refuse(http.StatusBadRequest, codeInvalidRequest, "a method override is not accepted")
		}
	}

	if _, ok := bodyEncoding(request); !ok {
		return refuseEncoding()
	}

	return nil
}

// bodyEncoding reports whether request's body is gzipped, and whether its
// Content-Encoding names only identity and gzip.
func bodyEncoding(request *http.Request) (gzipped, ok bool) {
	for _, value := range request.Header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(value, ",") {
			switch encoding = strings.TrimSpace(encoding); {
			case strings.EqualFold(encoding, "identity"):
			case strings.EqualFold(encoding, "gzip"):
				gzipped = true
			default:
				return gzipped, false
			}
		}
	}

	return gzipped, true
}

func refuseEncoding() *refusal {
	return refuse(http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
		"a request body encoding other than identity is not accepted, except gzip on git-upload-pack")
}

// refuseGzippedBody refuses a gzipped body on anything but git-upload-pack.
// git gzips an upload-pack body over 1 KiB, and the proxy never reads that
// body to decide; every other body may be read to decide.
func refuseGzippedBody(request *http.Request, classified Request) *refusal {
	if gzipped, _ := bodyEncoding(request); gzipped && classified.Kind != KindGitUpload {
		return refuseEncoding()
	}

	return nil
}

// checkPath accepts an escaped absolute path only when it has no "." or
// ".." segment, no empty segment, no encoded "/" or "\", and no encoded
// unreserved character, so every reader of it sees the same segments.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return errors.New("it is not an absolute path")
	}

	if path == "/" {
		return nil
	}

	for _, part := range strings.Split(path[1:], "/") {
		switch part {
		case "":
			return errors.New("it has an empty segment")
		case ".", "..":
			return errors.New("it has a dot segment")
		}

		if err := checkSegment(part); err != nil {
			return err
		}
	}

	return nil
}

func checkSegment(segment string) error {
	for index := 0; index < len(segment); index++ {
		switch character := segment[index]; character {
		case '\\':
			return errors.New("it has a backslash")
		case '%':
			if index+2 >= len(segment) || !isHex(segment[index+1]) || !isHex(segment[index+2]) {
				return errors.New("it has a malformed percent-encoding")
			}

			decoded := unhex(segment[index+1])<<4 | unhex(segment[index+2])

			switch {
			case decoded == '/' || decoded == '\\':
				return errors.New("it has an encoded slash or backslash")
			case isUnreserved(decoded):
				return errors.New("it has an encoded character that needs no encoding")
			}

			index += 2
		default:
			if character < 0x21 || character > 0x7e {
				return errors.New("it has a character that must be encoded")
			}
		}
	}

	return nil
}

func isHex(character byte) bool {
	return '0' <= character && character <= '9' || 'a' <= character && character <= 'f' || 'A' <= character && character <= 'F'
}

func unhex(character byte) byte {
	switch {
	case '0' <= character && character <= '9':
		return character - '0'
	case 'a' <= character && character <= 'f':
		return character - 'a' + 10
	default:
		return character - 'A' + 10
	}
}

// isUnreserved reports whether character is unreserved in RFC 3986.
func isUnreserved(character byte) bool {
	return 'a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' ||
		'0' <= character && character <= '9' || strings.IndexByte("-._~", character) >= 0
}

// refuseFormOverride refuses a form body with a _method field. The body
// read is put back for forwarding.
func refuseFormOverride(request *http.Request) *refusal {
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || request.Body == nil {
		return nil
	}

	if mediaType != "application/x-www-form-urlencoded" && mediaType != "multipart/form-data" {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxFormBytes+1))
	if err != nil {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "the request body can't be read")
	}

	if len(body) > maxFormBytes {
		return refuse(http.StatusRequestEntityTooLarge, codeTooLarge, "a form body larger than 1 MiB is not accepted")
	}

	_ = request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(body))

	if hasMethodField(mediaType, params, body) {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "a method override is not accepted")
	}

	return nil
}

// hasMethodField reports whether a form has a _method field, or can't be
// read (which fails closed).
func hasMethodField(mediaType string, params map[string]string, body []byte) bool {
	if mediaType == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(body))

		return err != nil || values.Has("_method")
	}

	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return false
		}

		if err != nil || part.FormName() == "_method" {
			return true
		}
	}
}

// classifyGit accepts only git smart HTTP: info/refs of upload-pack or
// receive-pack, and the two services themselves.
func classifyGit(classified Request, method string, query map[string][]string) (Request, *refusal) {
	notFound := refuse(http.StatusNotFound, codeNotFound, "only git smart HTTP is served on github.com")

	parts := strings.Split(strings.TrimPrefix(classified.Path, "/"), "/")
	if len(parts) < 3 {
		return Request{}, notFound
	}

	classified.Owner, classified.Repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	if classified.Repo == "" {
		return Request{}, notFound
	}

	service := strings.Join(parts[2:], "/")

	switch {
	case service == "info/refs" && (method == http.MethodGet || method == http.MethodHead):
		values := query["service"]
		if len(query) != 1 || len(values) != 1 {
			return Request{}, notFound
		}

		classified.Kind = KindGitInfoRefs
		service = values[0]
	case (service == "git-upload-pack" || service == "git-receive-pack") && method == http.MethodPost:
		classified.Kind = Kind(service)

		if len(query) != 0 {
			return Request{}, notFound
		}
	default:
		return Request{}, notFound
	}

	switch service {
	case "git-upload-pack":
		classified.Contents = permissions.LevelRead
	case "git-receive-pack":
		classified.Contents = permissions.LevelWrite
	default:
		return Request{}, notFound
	}

	return classified, nil
}
