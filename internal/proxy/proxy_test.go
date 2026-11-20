package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

const (
	audience   = "githubgate"
	validToken = "valid-projected-token"
	otherToken = "token-for-another-audience"
)

// recordingForwarder records every request handed to it, standing in for
// GitHub: a request it never sees is never sent anywhere.
type recordingForwarder struct {
	mu       sync.Mutex
	requests []proxy.Request
	headers  []http.Header
	callers  []auth.Identity
}

func (f *recordingForwarder) Forward(writer http.ResponseWriter, request *http.Request, caller auth.Identity, classified proxy.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, classified)
	f.headers = append(f.headers, request.Header.Clone())
	f.callers = append(f.callers, caller)

	writer.WriteHeader(http.StatusTeapot)
}

func (f *recordingForwarder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// tokenReviewAuthenticator is the token API's authenticator against a fake API
// server that knows validToken (with githubgate's audience) and otherToken
// (with another audience).
func tokenReviewAuthenticator(t *testing.T) auth.Authenticator {
	t.Helper()

	statuses := map[string]authenticationv1.TokenReviewStatus{
		validToken: serviceAccount("ci-a", "builder", audience),
		otherToken: serviceAccount("ci-a", "builder", "vault"),
	}

	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()

		status, known := statuses[review.Spec.Token]
		if !known {
			status = authenticationv1.TokenReviewStatus{Error: "invalid bearer token"}
		}

		// The API server answers with the intersection of the requested
		// audiences and the token's.
		if status.Authenticated && !contains(review.Spec.Audiences, status.Audiences[0]) {
			status = authenticationv1.TokenReviewStatus{Error: "token audiences are invalid"}
		}

		review.Status = status

		return true, review, nil
	})

	authenticator, err := auth.NewTokenReviewAuthenticator(client.AuthenticationV1().TokenReviews(), audience)
	require.NoError(t, err)

	return authenticator
}

func serviceAccount(namespace, name, tokenAudience string) authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{tokenAudience},
		User:          authenticationv1.UserInfo{Username: "system:serviceaccount:" + namespace + ":" + name},
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}

	return false
}

func newHandler(t *testing.T) (*proxy.Handler, *recordingForwarder) {
	t.Helper()

	forwarder := &recordingForwarder{}

	return proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder}), forwarder
}

func newRequest(method, host, target string) *http.Request {
	request := httptest.NewRequest(method, "https://"+host+"/", nil)
	request.RequestURI = target
	request.URL.Path, request.URL.RawQuery, _ = strings.Cut(target, "?")
	request.Host = host

	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

func TestProxyRefusesUnauthenticatedRequestsWithoutForwarding(t *testing.T) {
	tests := []struct {
		name          string
		host          string
		target        string
		authorization string
	}{
		{name: "no credential on the API host", host: proxy.HostAPI, target: "/repos/o/foo"},
		{name: "no credential on the git host", host: proxy.HostGit, target: "/o/foo.git/info/refs?service=git-upload-pack"},
		{name: "an invalid ServiceAccount token", host: proxy.HostAPI, target: "/repos/o/foo", authorization: "token not-a-real-token"},
		{name: "an invalid token in Basic", host: proxy.HostGit, target: "/o/foo/info/refs?service=git-upload-pack", authorization: basic("x-access-token", "not-a-real-token")},
		{name: "a token with the wrong audience", host: proxy.HostAPI, target: "/repos/o/foo", authorization: "Bearer " + otherToken},
		{name: "a token with the wrong audience in Basic", host: proxy.HostGit, target: "/o/foo/git-upload-pack", authorization: basic("x", otherToken)},
		{name: "an unknown operation without a credential", host: proxy.HostAPI, target: "/no/such/endpoint"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			method := http.MethodGet
			if strings.HasSuffix(test.target, "-pack") {
				method = http.MethodPost
			}

			request := newRequest(method, test.host, test.target)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}

			response := serve(handler, request)

			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.Equal(t, `Basic realm="GitHub"`, response.Header().Get("WWW-Authenticate"))
			assert.Zero(t, forwarder.count(), "the request was forwarded")
		})
	}
}

func basic(username, password string) string {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth(username, password)

	return request.Header.Get("Authorization")
}

func TestProxyAcceptsEveryCredentialFormAndRemovesIt(t *testing.T) {
	for _, authorization := range []string{"Bearer " + validToken, "token " + validToken, basic("x-access-token", validToken)} {
		t.Run(strings.Fields(authorization)[0], func(t *testing.T) {
			handler, forwarder := newHandler(t)

			request := newRequest(http.MethodGet, proxy.HostAPI, "/repos/o/foo")
			request.Header.Set("Authorization", authorization)

			response := serve(handler, request)

			require.Equal(t, http.StatusTeapot, response.Code)
			require.Equal(t, 1, forwarder.count())
			assert.Empty(t, forwarder.headers[0].Values("Authorization"))
			assert.Equal(t, auth.Identity{Namespace: "ci-a", ServiceAccount: "builder"}, forwarder.callers[0])
		})
	}
}

// authenticated returns a request with a valid credential.
func authenticated(method, host, target string) *http.Request {
	request := newRequest(method, host, target)
	request.Header.Set("Authorization", "token "+validToken)

	return request
}

func TestProxyChecksTheHostHeader(t *testing.T) {
	tests := []struct {
		host     string
		expected string
	}{
		{host: "api.github.com", expected: proxy.HostAPI},
		{host: "API.GitHub.com", expected: proxy.HostAPI},
		{host: "api.github.com:443", expected: proxy.HostAPI},
		{host: "uploads.github.com", expected: proxy.HostUploads},
		{host: "github.com", expected: proxy.HostGit},
		{host: "GitHub.com:443", expected: proxy.HostGit},
		{host: "api.github.com."},
		{host: "github.com.:443"},
		{host: "api.github.com:8443"},
		{host: "api.github.com:"},
		{host: "evil.example"},
		{host: "gist.github.com"},
		{host: "api.github.com.evil.example"},
		{host: ""},
	}

	for _, test := range tests {
		t.Run(test.host, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			target := "/repos/o/foo"
			if test.expected == proxy.HostGit {
				target = "/o/foo/info/refs?service=git-upload-pack"
			}

			response := serve(handler, authenticated(http.MethodGet, test.host, target))

			if test.expected == "" {
				assert.Equal(t, http.StatusMisdirectedRequest, response.Code)
				assert.Zero(t, forwarder.count())

				return
			}

			require.Equal(t, http.StatusTeapot, response.Code, response.Body.String())
			assert.Equal(t, test.expected, forwarder.requests[0].Host)
		})
	}
}

func TestProxyRefusesMethodsUpgradesOverridesAndEncodings(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		target   string
		header   http.Header
		body     string
		expected int
	}{
		{name: "CONNECT", method: http.MethodConnect, target: "/repos/o/foo", expected: http.StatusBadRequest},
		{name: "TRACE", method: http.MethodTrace, target: "/repos/o/foo", expected: http.StatusBadRequest},
		{name: "OPTIONS", method: http.MethodOptions, target: "/repos/o/foo", expected: http.StatusBadRequest},
		{name: "Upgrade", method: http.MethodGet, target: "/repos/o/foo", header: http.Header{"Upgrade": {"websocket"}}, expected: http.StatusBadRequest},
		{name: "empty Upgrade", method: http.MethodGet, target: "/repos/o/foo", header: http.Header{"Upgrade": {""}}, expected: http.StatusBadRequest},
		{name: "X-HTTP-Method-Override", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"X-Http-Method-Override": {"DELETE"}}, expected: http.StatusBadRequest},
		{name: "X-HTTP-Method", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"X-Http-Method": {"DELETE"}}, expected: http.StatusBadRequest},
		{name: "X-Method-Override", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"X-Method-Override": {"DELETE"}}, expected: http.StatusBadRequest},
		{name: "_method query", method: http.MethodPost, target: "/repos/o/foo/issues?_method=DELETE", expected: http.StatusBadRequest},
		{
			name: "_method form field", method: http.MethodPost, target: "/repos/o/foo/issues",
			header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, body: "title=x&_method=DELETE", expected: http.StatusBadRequest,
		},
		{
			name: "_method multipart field", method: http.MethodPost, target: "/repos/o/foo/issues",
			header: http.Header{"Content-Type": {"multipart/form-data; boundary=b"}},
			body:   "--b\r\nContent-Disposition: form-data; name=\"_method\"\r\n\r\nDELETE\r\n--b--\r\n", expected: http.StatusBadRequest,
		},
		{
			name: "form without _method", method: http.MethodPost, target: "/repos/o/foo/issues",
			header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, body: "title=x", expected: http.StatusTeapot,
		},
		{name: "gzip body", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"Content-Encoding": {"gzip"}}, expected: http.StatusUnsupportedMediaType},
		{name: "identity then gzip", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"Content-Encoding": {"identity, gzip"}}, expected: http.StatusUnsupportedMediaType},
		{name: "identity body", method: http.MethodPost, target: "/repos/o/foo/issues", header: http.Header{"Content-Encoding": {"identity"}}, expected: http.StatusTeapot},
		{name: "plain request", method: http.MethodPost, target: "/repos/o/foo/issues", expected: http.StatusTeapot},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			request := authenticated(test.method, proxy.HostAPI, test.target)
			request.Body = http.NoBody

			if test.body != "" {
				request.Body = readCloser(test.body)
			}

			for name, values := range test.header {
				request.Header[name] = values
			}

			response := serve(handler, request)

			assert.Equal(t, test.expected, response.Code, response.Body.String())

			if test.expected != http.StatusTeapot {
				assert.Zero(t, forwarder.count())
			}
		})
	}
}

// TestProxyAcceptsGzipOnlyOnGitUploadPack covers git compressing an
// upload-pack body: the proxy never reads that body, so it forwards it,
// while every body it reads to decide keeps refusing any encoding.
func TestProxyAcceptsGzipOnlyOnGitUploadPack(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		target   string
		encoding string
		expected int
	}{
		{name: "gzip upload-pack", host: proxy.HostGit, target: "/acme/foo.git/git-upload-pack", encoding: "gzip", expected: http.StatusTeapot},
		{name: "identity and gzip upload-pack", host: proxy.HostGit, target: "/acme/foo.git/git-upload-pack", encoding: "identity, GZIP", expected: http.StatusTeapot},
		{name: "deflate upload-pack", host: proxy.HostGit, target: "/acme/foo.git/git-upload-pack", encoding: "deflate", expected: http.StatusUnsupportedMediaType},
		{name: "gzip then br upload-pack", host: proxy.HostGit, target: "/acme/foo.git/git-upload-pack", encoding: "gzip, br", expected: http.StatusUnsupportedMediaType},
		{name: "gzip receive-pack", host: proxy.HostGit, target: "/acme/foo.git/git-receive-pack", encoding: "gzip", expected: http.StatusUnsupportedMediaType},
		{name: "gzip GraphQL", host: proxy.HostAPI, target: "/graphql", encoding: "gzip", expected: http.StatusUnsupportedMediaType},
		{name: "gzip ref-writing REST", host: proxy.HostAPI, target: "/repos/acme/foo/git/refs", encoding: "gzip", expected: http.StatusUnsupportedMediaType},
		{name: "gzip upload", host: proxy.HostUploads, target: "/repos/acme/foo/releases/1/assets?name=a", encoding: "gzip", expected: http.StatusUnsupportedMediaType},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			request := authenticated(http.MethodPost, test.host, test.target)
			request.Body = readCloser("compressed")
			request.Header.Set("Content-Encoding", test.encoding)

			response := serve(handler, request)

			assert.Equal(t, test.expected, response.Code, response.Body.String())

			if test.expected == http.StatusTeapot {
				require.Equal(t, 1, forwarder.count())
				assert.Equal(t, test.encoding, forwarder.headers[0].Get("Content-Encoding"))
			} else {
				assert.Zero(t, forwarder.count())
			}
		})
	}
}

func TestProxyRefusesAmbiguousPaths(t *testing.T) {
	refused := []string{
		"/repos/o/foo/./issues",
		"/repos/o/foo/../bar/issues",
		"/repos/o/foo/..",
		"/repos/o/foo/.",
		"/repos//o/foo",
		"/repos/o/foo/",
		"//repos/o/foo",
		"/repos/o%2Ffoo/issues",
		"/repos/o%2ffoo/issues",
		"/repos/o%5Cfoo/issues",
		"/repos/o%5cfoo/issues",
		"/repos/o/foo\\issues",
		"/repos/%6F/foo",
		"/repos/o/%2E%2E/issues",
		"/repos/o/%2e/issues",
		"/repos/o/fo%7E/issues",
		"/repos/o/fo%2D/issues",
		"/repos/o/fo%5F/issues",
		"/repos/o/foo%41/issues",
		"/repos/o/foo%30/issues",
		"/repos/o/foo%/issues",
		"/repos/o/foo%2/issues",
		"/repos/o/foo%zz/issues",
		"/repos/o/foo bar/issues",
		"repos/o/foo",
		"https://api.github.com/repos/o/foo",
		"*",
		"/repos/o/foo?a=%zz",
		"/repos/o/foo?a=1;b=2",
	}

	for _, target := range refused {
		t.Run(target, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(http.MethodGet, proxy.HostAPI, target))

			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Zero(t, forwarder.count())
		})
	}
}

func TestProxyRebuildsTheQuery(t *testing.T) {
	tests := []struct {
		target   string
		path     string
		rawQuery string
	}{
		{target: "/repos/o/foo/issues", path: "/repos/o/foo/issues", rawQuery: ""},
		{target: "/repos/o/foo/issues?state=open&per_page=5", path: "/repos/o/foo/issues", rawQuery: "per_page=5&state=open"},
		{target: "/repos/o/foo/issues?labels=a+b", path: "/repos/o/foo/issues", rawQuery: "labels=a+b"},
		{target: "/repos/o/foo/issues?labels=a%20b", path: "/repos/o/foo/issues", rawQuery: "labels=a+b"},
		{target: "/repos/o/foo/issues?flag", path: "/repos/o/foo/issues", rawQuery: "flag="},
		{target: "/repos/o/foo/contents/a%20b.txt", path: "/repos/o/foo/contents/a%20b.txt", rawQuery: ""},
	}

	for _, test := range tests {
		t.Run(test.target, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(http.MethodGet, proxy.HostAPI, test.target))

			require.Equal(t, http.StatusTeapot, response.Code, response.Body.String())
			assert.Equal(t, test.path, forwarder.requests[0].Path)
			assert.Equal(t, test.rawQuery, forwarder.requests[0].RawQuery)
		})
	}
}

func TestProxyClassifiesRESTRequests(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		method    string
		target    string
		operation string
		owner     string
		repo      string
	}{
		{name: "repository", host: proxy.HostAPI, method: http.MethodGet, target: "/repos/o/foo", operation: "repos/get", owner: "o", repo: "foo"},
		{name: "HEAD as GET", host: proxy.HostAPI, method: http.MethodHead, target: "/repos/o/foo", operation: "repos/get", owner: "o", repo: "foo"},
		{name: "create release", host: proxy.HostAPI, method: http.MethodPost, target: "/repos/o/foo/releases", operation: "repos/create-release", owner: "o", repo: "foo"},
		{name: "issue comment", host: proxy.HostAPI, method: http.MethodPost, target: "/repos/o/foo/issues/1/comments", operation: "issues/create-comment", owner: "o", repo: "foo"},
		{name: "slash parameter", host: proxy.HostAPI, method: http.MethodPatch, target: "/repos/o/foo/git/refs/heads/agent/x", operation: "git/update-ref", owner: "o", repo: "foo"},
		{name: "no repository", host: proxy.HostAPI, method: http.MethodGet, target: "/user", operation: "users/get-authenticated"},
		{name: "API root", host: proxy.HostAPI, method: http.MethodGet, target: "/", operation: "meta/root"},
		{name: "GET graphql is REST", host: proxy.HostAPI, method: http.MethodGet, target: "/repos/o/foo/issues", operation: "issues/list-for-repo", owner: "o", repo: "foo"},
		{name: "release asset upload", host: proxy.HostUploads, method: http.MethodPost, target: "/repos/o/foo/releases/1/assets?name=a.zip", operation: "repos/upload-release-asset", owner: "o", repo: "foo"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(test.method, test.host, test.target))

			require.Equal(t, http.StatusTeapot, response.Code, response.Body.String())

			classified := forwarder.requests[0]
			assert.Equal(t, proxy.KindREST, classified.Kind)
			assert.Equal(t, test.operation, classified.Operation)
			assert.Equal(t, test.owner, classified.Owner)
			assert.Equal(t, test.repo, classified.Repo)
			assert.Empty(t, classified.Contents)
		})
	}
}

func TestProxyClassifiesGraphQLRequests(t *testing.T) {
	handler, forwarder := newHandler(t)

	response := serve(handler, authenticated(http.MethodPost, proxy.HostAPI, "/graphql"))

	require.Equal(t, http.StatusTeapot, response.Code)
	assert.Equal(t, proxy.KindGraphQL, forwarder.requests[0].Kind)
}

func TestProxyRefusesUnknownRESTOperations(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		method string
		target string
	}{
		{name: "unknown path", host: proxy.HostAPI, method: http.MethodGet, target: "/no/such/endpoint"},
		{name: "unknown method on a known path", host: proxy.HostAPI, method: http.MethodPut, target: "/repos/o/foo/issues"},
		{name: "graphql on uploads", host: proxy.HostUploads, method: http.MethodPost, target: "/graphql"},
		{name: "graphql with GET", host: proxy.HostAPI, method: http.MethodGet, target: "/graphql"},
		{name: "unknown path on uploads", host: proxy.HostUploads, method: http.MethodPost, target: "/repos/o/foo/blobs"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(test.method, test.host, test.target))

			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.Zero(t, forwarder.count())

			var body struct {
				Message          string `json:"message"`
				Error            string `json:"error"`
				DocumentationURL string `json:"documentation_url"`
			}

			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, "unknown_operation", body.Error)
			assert.True(t, strings.HasPrefix(body.Message, "githubgate: "), body.Message)
			assert.NotEmpty(t, body.DocumentationURL)
		})
	}
}

func TestProxyClassifiesGitSmartHTTP(t *testing.T) {
	tests := []struct {
		method   string
		target   string
		kind     proxy.Kind
		repo     string
		contents permissions.Level
	}{
		{method: http.MethodGet, target: "/o/foo/info/refs?service=git-upload-pack", kind: proxy.KindGitInfoRefs, repo: "foo", contents: permissions.LevelRead},
		{method: http.MethodGet, target: "/o/foo.git/info/refs?service=git-upload-pack", kind: proxy.KindGitInfoRefs, repo: "foo", contents: permissions.LevelRead},
		{method: http.MethodGet, target: "/o/foo.git/info/refs?service=git-receive-pack", kind: proxy.KindGitInfoRefs, repo: "foo", contents: permissions.LevelWrite},
		{method: http.MethodPost, target: "/o/foo.git/git-upload-pack", kind: proxy.KindGitUpload, repo: "foo", contents: permissions.LevelRead},
		{method: http.MethodPost, target: "/o/foo/git-receive-pack", kind: proxy.KindGitReceive, repo: "foo", contents: permissions.LevelWrite},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(test.method, proxy.HostGit, test.target))

			require.Equal(t, http.StatusTeapot, response.Code, response.Body.String())

			classified := forwarder.requests[0]
			assert.Equal(t, test.kind, classified.Kind)
			assert.Equal(t, "o", classified.Owner)
			assert.Equal(t, test.repo, classified.Repo)
			assert.Equal(t, test.contents, classified.Contents)
		})
	}
}

func TestProxyServesOnlyGitSmartHTTPOnGitHubCom(t *testing.T) {
	tests := []struct {
		method string
		target string
	}{
		{method: http.MethodGet, target: "/o/foo"},
		{method: http.MethodGet, target: "/o/foo/archive/refs/heads/main.zip"},
		{method: http.MethodGet, target: "/repos/o/foo"},
		{method: http.MethodGet, target: "/o/foo/info/refs"},
		{method: http.MethodGet, target: "/o/foo/info/refs?service=git-upload-archive"},
		{method: http.MethodGet, target: "/o/foo/info/refs?service=git-upload-pack&service=git-receive-pack"},
		{method: http.MethodGet, target: "/o/foo/info/refs?service=git-upload-pack&x=1"},
		{method: http.MethodPost, target: "/o/foo/info/refs?service=git-upload-pack"},
		{method: http.MethodGet, target: "/o/foo/git-upload-pack"},
		{method: http.MethodPost, target: "/o/foo/git-upload-pack?x=1"},
		{method: http.MethodPost, target: "/o/.git/git-upload-pack"},
		{method: http.MethodPost, target: "/o/foo/bar/git-upload-pack"},
		{method: http.MethodPost, target: "/o/foo/info/git-upload-pack"},
		{method: http.MethodGet, target: "/o/foo/HEAD"},
		{method: http.MethodGet, target: "/o/foo/objects/info/packs"},
		{method: http.MethodGet, target: "/"},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			handler, forwarder := newHandler(t)

			response := serve(handler, authenticated(test.method, proxy.HostGit, test.target))

			assert.Equal(t, http.StatusNotFound, response.Code)
			assert.Equal(t, "text/plain; charset=utf-8", response.Header().Get("Content-Type"))
			assert.True(t, strings.HasPrefix(response.Body.String(), "githubgate: "))
			assert.Zero(t, forwarder.count())
		})
	}
}

func TestProxyGraphQLRefusalsCarryGraphQLErrors(t *testing.T) {
	handler, _ := newHandler(t)

	response := serve(handler, newRequest(http.MethodPost, proxy.HostAPI, "/graphql"))

	require.Equal(t, http.StatusUnauthorized, response.Code)

	var body struct {
		Error  string `json:"error"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "unauthenticated", body.Error)
	require.Len(t, body.Errors, 1)
	assert.True(t, strings.HasPrefix(body.Errors[0].Message, "githubgate: "))
}

func TestProxyRefusesWhenNotReady(t *testing.T) {
	forwarder := &recordingForwarder{}
	handler := proxy.NewHandler(proxy.Options{
		Authenticator: tokenReviewAuthenticator(t), Forwarder: forwarder, Ready: func() bool { return false },
	})

	response := serve(handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/o/foo"))

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Zero(t, forwarder.count())
}

func TestProxyWithoutForwarderForwardsNothing(t *testing.T) {
	handler := proxy.NewHandler(proxy.Options{Authenticator: tokenReviewAuthenticator(t)})

	response := serve(handler, authenticated(http.MethodGet, proxy.HostAPI, "/repos/o/foo"))

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func readCloser(body string) io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }
