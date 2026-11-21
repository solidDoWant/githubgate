//go:build live

package live_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// proxyEnv is the in-process proxy as a pod sees it: a tunnel that sends every
// HTTPS connection to the proxy, which presents a certificate for the GitHub
// hosts. gh and git are set to use the tunnel, so they run as in a pod and
// reach GitHub only through githubgate.
type proxyEnv struct {
	tunnelURL string
	caFile    string
}

func (l *liveEnv) startProxy(t *testing.T) proxyEnv {
	t.Helper()

	listener, caFile := l.serveProxy(t, "127.0.0.1:0", nil)

	return l.tunnelTo(t, listener, caFile)
}

// serveProxy starts the in-process proxy on address with a certificate for the
// GitHub hosts and returns its listener and the file holding its CA. observe,
// when set, sees every request the proxy receives before it decides.
func (l *liveEnv) serveProxy(t *testing.T, address string, observe func(*http.Request)) (net.Listener, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "githubgate live proxy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{proxy.HostAPI, proxy.HostUploads, proxy.HostGit},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certificate := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	caFile := filepath.Join(l.tmp, "proxy-ca.crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	forwarder, err := proxy.NewForwarder(proxy.ForwarderOptions{
		Rules: func(namespace, serviceAccount string) []*model.Rule {
			rules := l.store.Load().Rules(namespace, serviceAccount)
			if namespace == tenantNamespace {
				rules = append(rules[:len(rules):len(rules)], l.userRules[serviceAccount]...)
			}

			return rules
		},
		Tokens:     l.api,
		UserTokens: l.userTokens,
		Upstreams:  proxy.DefaultUpstreams(),
		Audit:      slog.New(slog.NewJSONHandler(io.MultiWriter(l.log, l.proxyAudit), nil)),
	})
	require.NoError(t, err)

	var handler http.Handler = proxy.NewHandler(proxy.Options{
		Authenticator: recordingAuthenticator{},
		Forwarder:     forwarder,
		Audit:         slog.New(slog.NewJSONHandler(io.MultiWriter(l.log, l.proxyAudit), nil)),
	})

	if observe != nil {
		inner := handler
		handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			observe(request)
			inner.ServeHTTP(writer, request)
		})
	}

	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)

	proxyServer := &http.Server{
		Handler:           handler,
		TLSConfig:         proxy.TLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certificate, nil }),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() { _ = proxyServer.ServeTLS(listener, "", "") }()

	t.Cleanup(func() { _ = proxyServer.Close() })

	return listener, caFile
}

// tunnelTo returns a CONNECT tunnel that sends every connection to listener.
func (l *liveEnv) tunnelTo(t *testing.T, listener net.Listener, caFile string) proxyEnv {
	t.Helper()

	tunnel := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(writer, "only CONNECT", http.StatusMethodNotAllowed)

			return
		}

		upstream, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)

			return
		}

		client, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()

			return
		}

		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		var wait sync.WaitGroup

		wait.Add(2)

		pipe := func(dst, src net.Conn) {
			defer wait.Done()

			_, _ = io.Copy(dst, src)
			_ = dst.Close()
		}

		go pipe(upstream, client)
		go pipe(client, upstream)

		wait.Wait()
	}))
	t.Cleanup(tunnel.Close)

	return proxyEnv{tunnelURL: tunnel.URL, caFile: caFile}
}

// runProxied runs a command as a ServiceAccount whose gh and git reach GitHub
// through the proxy, with the ServiceAccount token where a GitHub token goes.
func (l *liveEnv) runProxied(t *testing.T, tunnel proxyEnv, account, dir, name string, args ...string) (string, error) {
	t.Helper()

	home := filepath.Join(l.tmp, "proxied-home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	token := tenantNamespace + "/" + account
	helper := `!f() { echo username=x-access-token; echo password=` + token + `; }; f`

	cmd := commandAs(t, l.env(account), dir, name, args...)
	cmd.Env = append(cmd.Env,
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GH_TOKEN="+token,
		"HTTPS_PROXY="+tunnel.tunnelURL, "https_proxy="+tunnel.tunnelURL, "NO_PROXY=", "no_proxy=",
		"SSL_CERT_FILE="+tunnel.caFile,
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=http.proxy", "GIT_CONFIG_VALUE_0="+tunnel.tunnelURL,
		"GIT_CONFIG_KEY_1=http.sslCAInfo", "GIT_CONFIG_VALUE_1="+tunnel.caFile,
		"GIT_CONFIG_KEY_2=credential.helper", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=credential.helper", "GIT_CONFIG_VALUE_3="+helper,
	)

	out, err := cmd.CombinedOutput()
	l.log.printf(t, "$ %s %s\n%s", name, strings.Join(args, " "), out)

	return string(out), err
}

// publishBranch commits a new file on a new branch of a clone and publishes it
// with the client's push as the release account. It returns the branch and the
// clone's directory. The branch is deleted when the test ends.
func (l *liveEnv) publishBranch(t *testing.T) (string, string) {
	t.Helper()

	cloneURL := "https://github.com/" + l.owner + "/" + l.repo + ".git"

	_, err := l.run(t, releaseAccount, l.tmp, clientBinary, "configure")
	require.NoError(t, err)

	work := filepath.Join(l.tmp, "publish-"+strconv.Itoa(int(time.Now().UnixNano()%1e9)))
	_, err = l.run(t, releaseAccount, l.tmp, "git", "clone", cloneURL, work)
	require.NoError(t, err)

	branch := unique(t)
	_, err = l.run(t, releaseAccount, work, "git", "checkout", "-b", branch)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(work, branch+".txt"), []byte("githubgate live test\n"), 0o600))
	_, err = l.run(t, releaseAccount, work, "git", "add", branch+".txt")
	require.NoError(t, err)
	_, err = l.run(t, releaseAccount, work, "git", "commit", "-m", "githubgate live test")
	require.NoError(t, err)

	t.Cleanup(func() { l.deleteBranch(t, branch) })

	_, err = l.run(t, releaseAccount, work, clientBinary, "push", "origin", branch)
	require.NoError(t, err)

	return branch, work
}

// deleteBranch removes a branch with a token-mode token; a missing branch is
// not an error.
func (l *liveEnv) deleteBranch(t *testing.T, branch string) {
	t.Helper()

	fresh := l.requestToken(t, releaseAccount, map[string]any{
		"owner": l.owner, "repositories": []string{l.repo}, "permissions": map[string]string{"contents": "write"},
	})
	if fresh.token == "" {
		t.Errorf("could not get a token to remove branch %s", branch)

		return
	}

	l.github(t, fresh.token, http.MethodDelete, "/repos/"+l.owner+"/"+l.repo+"/git/refs/heads/"+url.PathEscape(branch), nil)
}

func TestLiveSignedCommitsAgainstGitHub(t *testing.T) {
	live := setup(t)

	branch, work := live.publishBranch(t)

	head, err := live.run(t, releaseAccount, work, "git", "rev-parse", "HEAD")
	require.NoError(t, err)

	sha := strings.TrimSpace(head)

	reader := live.requestToken(t, releaseAccount, map[string]any{
		"owner": live.owner, "repositories": []string{live.repo}, "permissions": map[string]string{"contents": "write"},
	})
	require.Equal(t, http.StatusOK, reader.status, reader.message)

	status, body := live.github(t, reader.token, http.MethodGet, "/repos/"+live.owner+"/"+live.repo+"/commits/"+sha, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	var commit struct {
		Commit struct {
			Message      string `json:"message"`
			Verification struct {
				Verified bool   `json:"verified"`
				Reason   string `json:"reason"`
			} `json:"verification"`
		} `json:"commit"`
	}
	require.NoError(t, json.Unmarshal(body, &commit))

	assert.True(t, commit.Commit.Verification.Verified, "the published commit is not verified: %s", commit.Commit.Verification.Reason)
	assert.Contains(t, commit.Commit.Message, "Co-authored-by:", "the original author is kept in a trailer")

	status, body = live.github(t, reader.token, http.MethodGet, "/repos/"+live.owner+"/"+live.repo+"/branches/"+url.PathEscape(branch), nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), sha, "the remote branch ends at the published commit")

	live.assertNoSecretsInTranscript(t)
}

func TestLiveProxyAgainstGitHub(t *testing.T) {
	live := setup(t)
	tunnel := live.startProxy(t)
	slug := live.owner + "/" + live.repo
	cloneURL := "https://github.com/" + slug + ".git"

	t.Run("a proxied pod reads an allowed repository with gh api", func(t *testing.T) {
		out, err := live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "repos/"+slug, "--jq", ".name")
		require.NoError(t, err)
		assert.Equal(t, live.repo, strings.TrimSpace(out))
	})

	t.Run("a denied operation is refused naming it while another operation on the repository works", func(t *testing.T) {
		tag := unique(t)
		out, err := live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "repos/"+slug+"/releases",
			"-f", "tag_name="+tag, "-f", "name="+tag)
		require.Error(t, err)
		assert.Contains(t, out, "repos/create-release")

		_, err = live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "repos/"+slug+"/issues", "--jq", "length")
		require.NoError(t, err)
	})

	t.Run("GraphQL queries work, a denied mutation is refused naming it and createPullRequest works", func(t *testing.T) {
		out, err := live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "graphql",
			"-f", `query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){id defaultBranchRef{name}}}`,
			"-f", "owner="+live.owner, "-f", "name="+live.repo)
		require.NoError(t, err)

		var queried struct {
			Data struct {
				Repository struct {
					ID               string `json:"id"`
					DefaultBranchRef struct {
						Name string `json:"name"`
					} `json:"defaultBranchRef"`
				} `json:"repository"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &queried))
		require.NotEmpty(t, queried.Data.Repository.ID)

		// The refusal comes before anything is forwarded, so the
		// repository id need not be right.
		out, err = live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "graphql",
			"-f", `query=mutation{createIssue(input:{repositoryId:"x",title:"x"}){issue{id}}}`)
		require.Error(t, err)
		assert.Contains(t, out, "createIssue")

		branch, _ := live.publishBranch(t)

		out, err = live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "graphql",
			"-f", `query=mutation($repo:ID!,$head:String!,$base:String!,$title:String!){createPullRequest(input:{repositoryId:$repo,headRefName:$head,baseRefName:$base,title:$title}){pullRequest{number}}}`,
			"-f", "repo="+queried.Data.Repository.ID, "-f", "head="+branch,
			"-f", "base="+queried.Data.Repository.DefaultBranchRef.Name, "-f", "title="+branch)
		require.NoError(t, err)

		var created struct {
			Data struct {
				CreatePullRequest struct {
					PullRequest struct {
						Number int `json:"number"`
					} `json:"pullRequest"`
				} `json:"createPullRequest"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &created))

		number := created.Data.CreatePullRequest.PullRequest.Number
		require.NotZero(t, number)

		// Pull requests cannot be deleted, so the test closes it.
		t.Cleanup(func() {
			_, _ = live.runProxied(t, tunnel, proxyAccount, live.tmp, "gh", "api", "-X", "PATCH",
				"repos/"+slug+"/pulls/"+strconv.Itoa(number), "-f", "state=closed")
		})
	})

	t.Run("with pushRefs a push to an allowed branch works and a push to another branch or a tag is refused naming the ref", func(t *testing.T) {
		work := filepath.Join(live.tmp, "proxied-work")
		_, err := live.runProxied(t, tunnel, pushAccount, live.tmp, "git", "clone", cloneURL, work)
		require.NoError(t, err)

		name := unique(t)
		_, err = live.runProxied(t, tunnel, pushAccount, work, "git", "checkout", "-b", name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(work, name+".txt"), []byte("githubgate live test\n"), 0o600))
		_, err = live.runProxied(t, tunnel, pushAccount, work, "git", "add", name+".txt")
		require.NoError(t, err)
		_, err = live.runProxied(t, tunnel, pushAccount, work, "git", "commit", "-m", "githubgate live test")
		require.NoError(t, err)

		allowed := "agent/" + name

		t.Cleanup(func() { live.deleteBranch(t, allowed) })

		_, err = live.runProxied(t, tunnel, pushAccount, work, "git", "push", "origin", "HEAD:refs/heads/"+allowed)
		require.NoError(t, err)

		out, err := live.runProxied(t, tunnel, pushAccount, work, "git", "push", "origin", "HEAD:refs/heads/"+name)
		require.Error(t, err)
		assert.Contains(t, out, "refs/heads/"+name)

		_, err = live.runProxied(t, tunnel, pushAccount, work, "git", "tag", name)
		require.NoError(t, err)
		out, err = live.runProxied(t, tunnel, pushAccount, work, "git", "push", "origin", "refs/tags/"+name)
		require.Error(t, err)
		assert.Contains(t, out, "refs/tags/"+name)

		// Nothing reached GitHub for the refused refs.
		remote, err := live.runProxied(t, tunnel, pushAccount, work, "git", "ls-remote", "origin")
		require.NoError(t, err)
		assert.Contains(t, remote, "refs/heads/"+allowed)
		assert.NotContains(t, remote, "refs/heads/"+name+"\n")
		assert.NotContains(t, remote, "refs/tags/"+name)
	})

	live.assertNoSecretsInTranscript(t)
}

// assertNoSecretsInTranscript fails the test when a token or a private key
// appears in anything the test printed, once the test has finished.
func (l *liveEnv) assertNoSecretsInTranscript(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		l.log.mu.Lock()
		defer l.log.mu.Unlock()

		output := l.log.buffer.String()

		for token := range l.log.tokens {
			assert.NotContains(t, output, token, "a token appears in the test's output")
		}

		assert.NotContains(t, output, "PRIVATE KEY", "a private key appears in the test's output")
	})
}

// commandAs builds a command that survives the test's context ending, so
// cleanups can run it.
func commandAs(t *testing.T, env []string, dir, name string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), name, args...)
	cmd.Dir = dir
	cmd.Env = env

	return cmd
}
