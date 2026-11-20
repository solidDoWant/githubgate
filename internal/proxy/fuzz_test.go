package proxy_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// FuzzProxyRequestTarget checks that whatever request target is sent, a
// request reaches the forwarder only with a path whose segments every
// reader splits the same way, and a query that is already in canonical form.
func FuzzProxyRequestTarget(f *testing.F) {
	for _, seed := range []string{
		"/repos/o/foo", "/repos/o/foo/git/refs/heads/a/b", "/repos/o/foo/./x", "/repos/o%2Ffoo",
		"/repos/o/%41", "/o/foo.git/info/refs?service=git-upload-pack", "/repos/o/foo?a=b&c=%20", "//x", "/%",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, target string) {
		for _, host := range []string{proxy.HostAPI, proxy.HostGit} {
			forwarder := &recordingForwarder{}
			handler := proxy.NewHandler(proxy.Options{Authenticator: acceptingAuthenticator{}, Forwarder: forwarder})

			request := newRequest(http.MethodGet, host, target)
			request.Header.Set("Authorization", "token x")
			serve(handler, request)

			if forwarder.count() == 0 {
				continue
			}

			classified := forwarder.requests[0]
			checkForwardedPath(t, classified.Path)

			query, err := url.ParseQuery(classified.RawQuery)
			if err != nil || query.Encode() != classified.RawQuery {
				t.Fatalf("query %q is not canonical", classified.RawQuery)
			}
		}
	})
}

func checkForwardedPath(t *testing.T, path string) {
	t.Helper()

	if !strings.HasPrefix(path, "/") {
		t.Fatalf("path %q is not absolute", path)
	}

	if path == "/" {
		return
	}

	for _, segment := range strings.Split(path[1:], "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			t.Fatalf("path %q has a segment that doesn't decode", path)
		}

		if segment == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\") {
			t.Fatalf("path %q has an ambiguous segment %q", path, segment)
		}

		for index := strings.IndexByte(segment, '%'); index >= 0; index = nextPercent(segment, index) {
			value, err := strconv.ParseUint(segment[index+1:index+3], 16, 8)
			if err != nil || strings.IndexByte(unreserved, byte(value)) >= 0 {
				t.Fatalf("path %q encodes %q, which needs no encoding", path, segment[index:index+3])
			}
		}
	}
}

// unreserved are the characters RFC 3986 never needs encoded.
const unreserved = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"

func nextPercent(segment string, index int) int {
	next := strings.IndexByte(segment[index+1:], '%')
	if next < 0 {
		return -1
	}

	return index + 1 + next
}

// acceptingAuthenticator authenticates every request, so fuzzing reaches
// the classification.
type acceptingAuthenticator struct{}

func (acceptingAuthenticator) Authenticate(context.Context, *http.Request) (auth.Identity, error) {
	return auth.Identity{Namespace: "ci-a", ServiceAccount: "builder"}, nil
}

// FuzzReceivePackCommands checks that whatever a push sends, it reaches
// GitHub only unchanged and only when every ref it writes matches pushRefs;
// anything else is refused with nothing forwarded.
func FuzzReceivePackCommands(f *testing.F) {
	for _, seed := range []string{
		pushBody("report-status side-band-64k", "refs/heads/agent/x"),
		pushBody("report-status", "refs/heads/agent/x", "refs/heads/master"),
		pkt("shallow "+oldID+"\n", oldID+" "+newID+" refs/heads/agent/x\x00report-status") + flush + packData,
		pkt("push-cert\x00report-status", "push-cert-end\n") + flush,
		"zzzz", "", flush, "0004" + flush, "fff0",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		forwarding := newForwardingProxy(t, agentRule())

		response := serve(forwarding.handler, pushRequest(body))

		received := forwarding.upstream.requests()
		if len(received) == 0 {
			return
		}

		if received[0].body != body {
			t.Fatalf("the push reached GitHub changed: %q became %q", body, received[0].body)
		}

		if response.Code != http.StatusOK {
			t.Fatalf("a forwarded push answered %d", response.Code)
		}

		lines := forwarding.auditLines(t)
		if len(lines) != 1 {
			t.Fatalf("%d audit lines", len(lines))
		}

		refs, _ := lines[0]["refs"].([]any)
		if len(refs) == 0 {
			t.Fatalf("a push writing no known ref was forwarded: %q", body)
		}

		for _, ref := range refs {
			name, _ := ref.(string)
			if !strings.HasPrefix(name, "refs/heads/agent/") || !strings.Contains(body, name) {
				t.Fatalf("ref %q outside pushRefs was forwarded", name)
			}
		}
	})
}
