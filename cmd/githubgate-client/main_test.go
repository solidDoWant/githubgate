package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunPrintsTheBuildVersion(t *testing.T) {
	original := version
	version = "1.2.3"

	t.Cleanup(func() { version = original })

	out, _, err := runClient(t, nil, "", "version")
	require.NoError(t, err)
	require.Equal(t, "githubgate-client 1.2.3\n", out)
}

// fakeAPI is an HTTPS token API that records requests.
type fakeAPI struct {
	server   *httptest.Server
	mu       sync.Mutex
	bearers  []string
	bodies   []tokenRequest
	handler  func(call int, w http.ResponseWriter)
	envelope map[string]string
}

func newFakeAPI(t *testing.T, handler func(call int, w http.ResponseWriter)) *fakeAPI {
	t.Helper()

	api := &fakeAPI{handler: handler}
	api.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()

		assert.Equal(t, "/v1/token", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)

		var body tokenRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		api.bearers = append(api.bearers, r.Header.Get("Authorization"))
		api.bodies = append(api.bodies, body)
		api.handler(len(api.bodies), w)
	}))
	t.Cleanup(api.server.Close)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.server.Certificate().Raw}), 0o600))

	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("sa-one\n"), 0o600))

	api.envelope = map[string]string{
		"GITHUBGATE_URL":        api.server.URL,
		"GITHUBGATE_CA_FILE":    caFile,
		"GITHUBGATE_TOKEN_FILE": tokenFile,
	}

	return api
}

func (a *fakeAPI) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.bodies)
}

func respondToken(token string) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"token":"` + token + `","expiresAt":"2030-01-01T00:00:00Z"}`))
	}
}

func respondError(status int, code, message string) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
	}
}

// runClient runs the client with the given env and returns stdout and stderr.
func runClient(t *testing.T, vars map[string]string, stdin string, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	env := &environment{
		stdin:  strings.NewReader(stdin),
		stdout: &stdout,
		stderr: &stderr,
		getenv: func(key string) string { return vars[key] },
		sleep:  func(context.Context, int) error { return nil },
	}

	err := run(t.Context(), env, args)

	return stdout.String(), stderr.String(), err
}

func TestTokenPrintsOnlyTheTokenAndSendsTheRequest(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))

	out, _, err := runClient(t, api.envelope, "", "token", "--repo", "foo", "--permission", "contents=read", "--installation", "ns/inst")
	require.NoError(t, err)
	require.Equal(t, "ghs_abc\n", out)
	require.Equal(t, []tokenRequest{{
		Installation: "ns/inst",
		Repositories: []string{"foo"},
		Permissions:  map[string]string{"contents": "read"},
	}}, api.bodies)
	require.Equal(t, []string{"Bearer sa-one"}, api.bearers)
}

func TestTokenRejectsMalformedPermission(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))

	_, _, err := runClient(t, api.envelope, "", "token", "--permission", "contents")
	require.Error(t, err)
	require.Zero(t, api.calls())
}

func TestServiceAccountTokenFileIsReadOnEveryCall(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))

	_, _, err := runClient(t, api.envelope, "", "token")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(api.envelope["GITHUBGATE_TOKEN_FILE"], []byte("sa-two"), 0o600))
	_, _, err = runClient(t, api.envelope, "", "token")
	require.NoError(t, err)

	require.Equal(t, []string{"Bearer sa-one", "Bearer sa-two"}, api.bearers)
}

func TestHTTPURLIsRefusedBeforeAnythingIsSent(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))
	api.envelope["GITHUBGATE_URL"] = strings.Replace(api.server.URL, "https://", "http://", 1)

	_, _, err := runClient(t, api.envelope, "", "token")
	require.ErrorContains(t, err, "https")
	require.Zero(t, api.calls())
}

func TestServerCertificateOutsideTheCAFileIsRejected(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	otherCA := filepath.Join(t.TempDir(), "other.crt")
	require.NoError(t, os.WriteFile(otherCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	api.envelope["GITHUBGATE_CA_FILE"] = otherCA

	_, _, err = runClient(t, api.envelope, "", "token")
	require.Error(t, err)
	require.Zero(t, api.calls())
}

func TestTokenRequestIsRetriedOnceOnNotReady(t *testing.T) {
	tests := map[string]struct {
		handler   func(int, http.ResponseWriter)
		wantCalls int
		wantErr   bool
	}{
		"not_ready then success": {
			handler: func(call int, w http.ResponseWriter) {
				if call == 1 {
					respondError(http.StatusServiceUnavailable, "not_ready", "starting")(call, w)

					return
				}

				respondToken("ghs_abc")(call, w)
			},
			wantCalls: 2,
		},
		"not_ready twice stops after one retry": {
			handler:   respondError(http.StatusServiceUnavailable, "not_ready", "starting"),
			wantCalls: 2,
			wantErr:   true,
		},
		"other unavailable is not retried": {
			handler:   respondError(http.StatusServiceUnavailable, "github_unavailable", "down"),
			wantCalls: 1,
			wantErr:   true,
		},
		"denied is not retried": {
			handler:   respondError(http.StatusForbidden, "denied", "no"),
			wantCalls: 1,
			wantErr:   true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t, test.handler)

			_, _, err := runClient(t, api.envelope, "", "token")
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, test.wantCalls, api.calls())
		})
	}
}

func TestTokenRequestIsRetriedOnceOnConnectionError(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))
	api.server.Close()

	var slept []int

	var stdout bytes.Buffer

	env := &environment{
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &bytes.Buffer{},
		getenv: func(key string) string { return api.envelope[key] },
		sleep: func(_ context.Context, millis int) error {
			slept = append(slept, millis)

			return nil
		},
	}

	require.Error(t, run(t.Context(), env, []string{"token"}))
	require.Len(t, slept, 1)
	require.GreaterOrEqual(t, slept[0], 200)
	require.LessOrEqual(t, slept[0], 500)
}

func TestGitCredentialGet(t *testing.T) {
	tests := map[string]struct {
		input        string
		wantRequest  *tokenRequest
		wantOutput   string
		wantStderr   string
		handler      func(int, http.ResponseWriter)
		wantQuitOnly bool
	}{
		"path with .git": {
			input:       "protocol=https\nhost=github.com\npath=acme/foo.git\n\n",
			wantRequest: &tokenRequest{Owner: "acme", Repositories: []string{"foo"}},
			wantOutput:  "username=x-access-token\npassword=ghs_abc\npassword_expiry_utc=1893456000\n",
		},
		"no path": {
			input:       "protocol=https\nhost=github.com\n\n",
			wantRequest: &tokenRequest{},
			wantOutput:  "username=x-access-token\npassword=ghs_abc\npassword_expiry_utc=1893456000\n",
		},
		"other host is left to other helpers": {
			input:      "protocol=https\nhost=example.com\npath=acme/foo\n\n",
			wantOutput: "",
		},
		"http protocol is ignored": {
			input:      "protocol=http\nhost=github.com\n\n",
			wantOutput: "",
		},
		"refusal prints quit and the reason": {
			input:       "protocol=https\nhost=github.com\npath=acme/foo\n\n",
			handler:     respondError(http.StatusForbidden, "denied", "namespace ci may not use acme/foo"),
			wantRequest: &tokenRequest{Owner: "acme", Repositories: []string{"foo"}},
			wantOutput:  "quit=1\n",
			wantStderr:  "namespace ci may not use acme/foo",
		},
		"path without a repository is refused": {
			input:      "protocol=https\nhost=github.com\npath=acme\n\n",
			wantOutput: "quit=1\n",
			wantStderr: "cannot find a repository",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			handler := test.handler
			if handler == nil {
				handler = respondToken("ghs_abc")
			}

			api := newFakeAPI(t, handler)

			out, stderr, err := runClient(t, api.envelope, test.input, "git-credential", "get")
			require.NoError(t, err)
			require.Equal(t, test.wantOutput, out)
			require.Contains(t, stderr, test.wantStderr)
			require.NotContains(t, stderr, "sa-one")

			if test.wantRequest == nil {
				require.Zero(t, api.calls())
			} else {
				require.Equal(t, []tokenRequest{*test.wantRequest}, api.bodies)
			}
		})
	}
}

func TestGitCredentialStoreAndEraseDoNothing(t *testing.T) {
	api := newFakeAPI(t, respondToken("ghs_abc"))

	for _, operation := range []string{"store", "erase"} {
		out, _, err := runClient(t, api.envelope, "protocol=https\nhost=github.com\npassword=x\n\n", "git-credential", operation)
		require.NoError(t, err)
		require.Empty(t, out)
	}

	require.Zero(t, api.calls())
}

func TestConfigureKeepsOtherGitConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	gitConfig := func(args ...string) string {
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"config", "--global"}, args...)...).Output()
		require.NoError(t, err)

		return string(out)
	}

	gitConfig("user.name", "Someone")
	gitConfig("--add", helperKey, "store")

	for range 2 {
		_, _, err := runClient(t, map[string]string{"GIT_ASKPASS": "/bin/askpass"}, "", "configure")
		require.NoError(t, err)
	}

	require.Equal(t, "Someone\n", gitConfig("user.name"))
	require.Equal(t, "\n"+helperValue+"\n", gitConfig("--get-all", helperKey))
	require.Equal(t, "true\n", gitConfig(useHTTPPathKey))
}

func TestConfigureWarnsAboutAskPass(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	_, stderr, err := runClient(t, map[string]string{"GIT_ASKPASS": "/bin/askpass"}, "", "configure")
	require.NoError(t, err)
	require.Contains(t, stderr, "GIT_ASKPASS")
}

func TestGHGetsAFreshTokenOnEveryCall(t *testing.T) {
	var mu sync.Mutex

	counter := 0
	api := newFakeAPI(t, func(_ int, w http.ResponseWriter) {
		mu.Lock()
		counter++
		token := "ghs_" + string(rune('a'+counter))
		mu.Unlock()
		respondToken(token)(0, w)
	})

	bin := t.TempDir()
	stub := "#!/bin/sh\necho \"$GH_TOKEN $*\"\n[ \"$1\" = fail ] && exit 7\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "")

	out, _, err := runClient(t, api.envelope, "", "gh", "--repo", "acme/foo", "--", "issue", "comment")
	require.NoError(t, err)
	require.Equal(t, "ghs_b issue comment\n", out)

	out, _, err = runClient(t, api.envelope, "", "gh", "--repo", "acme/foo", "--", "pr", "create")
	require.NoError(t, err)
	require.Equal(t, "ghs_c pr create\n", out)

	require.Equal(t, tokenRequest{Owner: "acme", Repositories: []string{"foo"}}, api.bodies[0])

	_, _, err = runClient(t, api.envelope, "", "gh", "--repo", "acme/foo", "--", "fail")
	require.Equal(t, exitError(7), err)
}

func TestParseGitHubRemote(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/foo.git":        "acme/foo",
		"https://github.com/acme/foo":            "acme/foo",
		"https://user@github.com/acme/foo/":      "acme/foo",
		"git@github.com:acme/foo.git":            "acme/foo",
		"https://gitlab.com/acme/foo.git":        "",
		"https://github.com/acme":                "",
		"https://github.com/acme/foo/extra/more": "",
	}

	for remote, want := range tests {
		t.Run(remote, func(t *testing.T) {
			require.Equal(t, want, parseGitHubRemote(remote))
		})
	}
}

func TestUnknownCommandFails(t *testing.T) {
	_, _, err := runClient(t, nil, "", "nope")
	require.Error(t, err)
}

func TestShutdownContextCancelsOnTerminationSignals(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := shutdownContext(t.Context())
			defer stop()

			require.NoError(t, syscall.Kill(os.Getpid(), sig))

			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("context was not cancelled by the signal")
			}
		})
	}
}
