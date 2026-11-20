package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proxyEnvelope is the token API envelope plus a proxy CA file. The token
// API fails the test if it is called, because proxy mode never asks it.
func proxyEnvelope(t *testing.T, caCert []byte) (*fakeAPI, map[string]string) {
	t.Helper()

	api := newFakeAPI(t, func(int, http.ResponseWriter) { t.Error("proxy mode called the token API") })

	caFile := filepath.Join(t.TempDir(), "proxy-ca.crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert}), 0o600))

	vars := map[string]string{}
	for key, value := range api.envelope {
		vars[key] = value
	}

	vars["GITHUBGATE_PROXY_CA_FILE"] = caFile

	return api, vars
}

func TestProxyCredentialHelperAnswersTheServiceAccountTokenOnlyForGitHub(t *testing.T) {
	tests := map[string]struct {
		input      string
		wantOutput string
	}{
		"github.com over https with a path": {
			input:      "protocol=https\nhost=github.com\npath=acme/foo.git\n\n",
			wantOutput: "username=x-access-token\npassword=sa-one\n",
		},
		"github.com over https without a path": {
			input:      "protocol=https\nhost=github.com\n\n",
			wantOutput: "username=x-access-token\npassword=sa-one\n",
		},
		"another host": {
			input: "protocol=https\nhost=example.com\npath=acme/foo.git\n\n",
		},
		"a host that only starts with github.com": {
			input: "protocol=https\nhost=github.com.example.com\n\n",
		},
		"github.com on another port": {
			input: "protocol=https\nhost=github.com:8443\n\n",
		},
		"a subdomain of github.com": {
			input: "protocol=https\nhost=gist.github.com\n\n",
		},
		"github.com over http": {
			input: "protocol=http\nhost=github.com\n\n",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api, vars := proxyEnvelope(t, []byte("unused"))

			out, stderr, err := runClient(t, vars, test.input, "git-credential", "--proxy", "get")
			require.NoError(t, err)
			assert.Equal(t, test.wantOutput, out)
			assert.NotContains(t, stderr, "sa-one")
			assert.Zero(t, api.calls())
		})
	}
}

func TestProxyCredentialHelperReadsTheTokenFileOnEveryCall(t *testing.T) {
	_, vars := proxyEnvelope(t, []byte("unused"))
	input := "protocol=https\nhost=github.com\n\n"

	out, _, err := runClient(t, vars, input, "git-credential", "--proxy", "get")
	require.NoError(t, err)
	assert.Equal(t, "username=x-access-token\npassword=sa-one\n", out)

	require.NoError(t, os.WriteFile(vars["GITHUBGATE_TOKEN_FILE"], []byte("sa-two\n"), 0o600))

	out, _, err = runClient(t, vars, input, "git-credential", "--proxy", "get")
	require.NoError(t, err)
	assert.Equal(t, "username=x-access-token\npassword=sa-two\n", out)

	require.NoError(t, os.Remove(vars["GITHUBGATE_TOKEN_FILE"]))

	out, _, err = runClient(t, vars, input, "git-credential", "--proxy", "get")
	require.NoError(t, err)
	assert.Equal(t, "quit=1\n", out, "without a token git must stop instead of asking others")
}

func TestConfigureProxySetsTheProxyHelperAndOnlyTheProxyCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	gitConfig := func(args ...string) string {
		out, _ := exec.CommandContext(t.Context(), "git", append([]string{"config", "--global"}, args...)...).Output()

		return string(out)
	}

	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	_, vars := proxyEnvelope(t, server.Certificate().Raw)

	_, _, err := runClient(t, map[string]string{"GITHUBGATE_PROXY_CA_FILE": filepath.Join(t.TempDir(), "missing.crt")}, "", "configure", "--proxy")
	require.Error(t, err, "configure --proxy needs the proxy CA")
	assert.Empty(t, gitConfig("--get-all", helperKey), "nothing is set without the proxy CA")

	for range 2 {
		out, _, err := runClient(t, vars, "", "configure", "--proxy")
		require.NoError(t, err)
		assert.Contains(t, out, "proxy")
	}

	assert.Equal(t, "\n!githubgate-client git-credential --proxy\n", gitConfig("--get-all", helperKey))
	assert.Equal(t, vars["GITHUBGATE_PROXY_CA_FILE"]+"\n", gitConfig("--get-all", sslCAInfoKey))
}

func TestGHProxyRunsGHWithTheServiceAccountTokenAndOnlyTheProxyCA(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	api, vars := proxyEnvelope(t, server.Certificate().Raw)

	bin := t.TempDir()
	stub := "#!/bin/sh\necho \"$GH_TOKEN|$SSL_CERT_FILE|$(ls -A \"$SSL_CERT_DIR\" | wc -l)|$*\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "")
	t.Setenv("SSL_CERT_DIR", "/etc/ssl/certs")

	out, _, err := runClient(t, vars, "", "gh", "--proxy", "--", "api", "repos/acme/foo")
	require.NoError(t, err)
	assert.Equal(t, "sa-one|"+vars["GITHUBGATE_PROXY_CA_FILE"]+"|0|api repos/acme/foo\n", out)
	assert.Zero(t, api.calls())

	vars["GITHUBGATE_PROXY_CA_FILE"] = filepath.Join(t.TempDir(), "missing.crt")
	out, _, err = runClient(t, vars, "", "gh", "--proxy", "--", "api", "repos/acme/foo")
	require.Error(t, err, "gh does not run without the proxy CA")
	assert.Empty(t, out)
}

// otherCA is a self-signed certificate that signed nothing the tests serve.
func otherCA(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	return der
}

// proxyFront stands in for githubgate's proxy: it accepts the ServiceAccount
// token and forwards to the fake GitHub with an installation token.
func (f *pushFixture) proxyFront(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()

	target, err := url.Parse(f.api.URL)
	require.NoError(t, err)

	installationToken := f.mintToken(f.key)
	forward := httputil.NewSingleHostReverseProxy(target)

	var (
		mu    sync.Mutex
		auths []string
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()

		r.Header.Set("Authorization", "token "+installationToken)
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	return server, &auths
}

func TestPushThroughTheProxySendsTheServiceAccountTokenToTheProxyOnly(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "-b", "agent/x")
	fix.commit("a.txt", "a\n", "add a")

	front, auths := fix.proxyFront(t)
	api, vars := proxyEnvelope(t, front.Certificate().Raw)
	vars["GITHUBGATE_GITHUB_API_URL"] = front.URL

	out, _, err := runClient(t, vars, "", "push", "--proxy", "origin", "agent/x")
	require.NoError(t, err)
	assert.Contains(t, out, "published 1 commit(s)")
	assert.Equal(t, fix.remoteTip("agent/x"), gitIn(t, fix.work, "rev-parse", "HEAD"))
	assert.Zero(t, api.calls(), "proxy mode never asks for a token")

	require.NotEmpty(t, *auths)

	for _, auth := range *auths {
		assert.Equal(t, "Bearer sa-one", auth)
	}
}

func TestPushThroughTheProxyRefusesAServerOutsideTheProxyCA(t *testing.T) {
	fix := newPushFixture(t)
	gitIn(t, fix.work, "checkout", "-q", "-b", "agent/x")
	fix.commit("a.txt", "a\n", "add a")

	front, auths := fix.proxyFront(t)

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the ServiceAccount token went over http: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(plain.Close)

	tests := map[string]struct {
		caCert []byte
		root   string
	}{
		"a certificate the proxy CA did not sign": {caCert: otherCA(t), root: front.URL},
		"an http API root":                        {caCert: front.Certificate().Raw, root: plain.URL},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, vars := proxyEnvelope(t, test.caCert)
			vars["GITHUBGATE_GITHUB_API_URL"] = test.root

			_, _, err := runClient(t, vars, "", "push", "--proxy")
			require.Error(t, err)
			assert.Empty(t, *auths, "nothing reached the server")
			assert.Error(t, exec.CommandContext(t.Context(), "git", "-C", fix.bare, "rev-parse", "--verify", "--quiet", "refs/heads/agent/x").Run(), "the branch was not published")
		})
	}
}
