package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/model"
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

	// One forwarder per worker, with an in-process upstream, so each input
	// costs no server or connection.
	var (
		forwarded [][]byte
		audit     bytes.Buffer
	)

	upstream := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}

		forwarded = append(forwarded, data)

		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: request}, nil
	})

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules:     func(string, string) []*model.Rule { return []*model.Rule{agentRule()} },
		Tokens:    &fakeTokens{},
		Upstreams: proxy.DefaultUpstreams(),
		Transport: upstream,
		Audit:     slog.New(slog.NewJSONHandler(&audit, nil)),
	})
	if err != nil {
		f.Fatal(err)
	}

	handler := proxy.NewHandler(proxy.Options{Authenticator: acceptingAuthenticator{}, Forwarder: forwarder})

	f.Fuzz(func(t *testing.T, body string) {
		forwarded, audit = nil, bytes.Buffer{}

		response := serve(handler, pushRequest(body))

		if len(forwarded) == 0 {
			return
		}

		if string(forwarded[0]) != body {
			t.Fatalf("the push reached GitHub changed: %q became %q", body, forwarded[0])
		}

		if response.Code != http.StatusOK {
			t.Fatalf("a forwarded push answered %d", response.Code)
		}

		var line struct {
			Refs []string `json:"refs"`
		}

		if err := json.Unmarshal(audit.Bytes(), &line); err != nil {
			t.Fatalf("the audit line can't be read: %v", err)
		}

		// Only git's probe, a body that is exactly one flush packet, may be
		// forwarded without a ref: it names none and carries no pack.
		if len(line.Refs) == 0 && body != "0000" {
			t.Fatalf("a push writing no known ref was forwarded: %q", body)
		}

		for _, ref := range line.Refs {
			if !strings.HasPrefix(ref, "refs/heads/agent/") {
				t.Fatalf("ref %q outside pushRefs was forwarded", ref)
			}
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
