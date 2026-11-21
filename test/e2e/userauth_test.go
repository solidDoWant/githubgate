//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	userClientID  = "e2e-user-client"
	userLogin     = "alice"
	userCaller    = "ci-user"
	signingKeyOps = "[users/create-ssh-signing-key-for-authenticated-user, users/delete-ssh-signing-key-for-authenticated-user]"
)

// installUserAuthorizations gives the App a client ID, creates an authorization
// the user will approve and one nobody will, and a policy with a user rule for
// each next to an installation rule. It starts the pod that runs as that
// policy's ServiceAccount.
func (c cluster) installUserAuthorizations(t *testing.T) {
	t.Helper()

	c.control(t, "/oauth-apps", map[string]any{"client_id": userClientID, "device_flow": true})
	c.kubectl(t, "", "patch", "githubapp/e2e", "-n", gateNamespace, "--type=merge", "-p", fmt.Sprintf(`{"spec":{"clientID":%q}}`, userClientID))

	c.apply(t, fmt.Sprintf(`
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubUserAuthorization
metadata:
  name: alice
  namespace: %[1]s
  annotations: {githubgate.soliddowant.dev/authorize: "first"}
spec:
  appRef: {name: e2e}
  login: %[3]s
  ceiling:
    operations: %[4]s
  allowedNamespaces:
    selector: {matchLabels: {e2e-client: "true"}}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubUserAuthorization
metadata: {name: nobody, namespace: %[1]s}
spec:
  appRef: {name: e2e}
  login: nobody
  ceiling:
    operations: [users/list-ssh-signing-keys-for-authenticated-user]
  allowedNamespaces:
    selector: {matchLabels: {e2e-client: "true"}}
---
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata: {name: %[5]s, namespace: %[2]s}
spec:
  serviceAccounts: [%[5]s]
  rules:
    - name: keys
      userAuthorizationRef: {namespace: %[1]s, name: alice}
      proxy: {enabled: true, allowOperations: %[4]s}
    - name: pending
      userAuthorizationRef: {namespace: %[1]s, name: nobody}
      proxy: {enabled: true, allowOperations: [users/list-ssh-signing-keys-for-authenticated-user]}
    - name: repo
      installationRef: {namespace: %[1]s, name: acme}
      repositories: [%[6]s]
      permissions: {contents: read}
      proxy: {enabled: true}
`, gateNamespace, callerNamespace, userLogin, signingKeyOps, userCaller, repository))

	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", callerNamespace, "githubaccesspolicy/"+userCaller, "--timeout=2m")
	c.startCallerPod(t, userCaller, true)
}

// authorizeThroughDeviceFlow waits for the user code in the status of the
// authorization, approves it as login with the fake's test hook, and waits
// for the authorization to be Ready.
func (c cluster) authorizeThroughDeviceFlow(ctx context.Context, t *testing.T, name, login string) {
	t.Helper()

	var code string

	require.Eventually(t, func() bool {
		out, err := c.tryKubectl(ctx, "", "get", "githubuserauthorization/"+name, "-n", gateNamespace, "-o", "jsonpath={.status.authorization.userCode}")
		code = strings.TrimSpace(out)

		return err == nil && code != ""
	}, 2*time.Minute, time.Second, "the status shows a user code")

	c.control(t, "/device-codes/approve", map[string]any{"user_code": code, "login": login})
	c.kubectl(t, "", "wait", "--for=condition=Ready", "-n", gateNamespace, "githubuserauthorization/"+name, "--timeout=2m")
}

// runUserAuthorizationScenarios runs against the cluster of the main suite. It
// is called with a function that records the output of every call, for the
// leak search.
func (c cluster) runUserAuthorizationScenarios(ctx context.Context, t *testing.T, recordOutput func(string)) {
	t.Helper()

	c.installUserAuthorizations(t)

	// call runs one request through the proxy in the user pod and returns
	// the status line, the headers and the body.
	call := func(t *testing.T, method, target, body string) string {
		t.Helper()

		data := ""
		if body != "" {
			data = fmt.Sprintf("-H 'Content-Type: application/json' --data '%s'", body)
		}

		out, err := c.exec(ctx, callerNamespace, userCaller, fmt.Sprintf(`curl -sS -i --cacert /var/run/secrets/githubgate-proxy/ca.crt -w '\nstatus=%%{http_code}\n' -X %s -H "Authorization: token $(cat /var/run/secrets/githubgate/token)" -H 'Accept: application/vnd.github+json' %s '%s'`,
			method, data, target))
		recordOutput(out)
		require.NoError(t, err, out)

		return out
	}

	t.Run("a request a user rule covers gets 503 naming the authorization while it is not Ready", func(t *testing.T) {
		out := call(t, "GET", "https://api.github.com/user/ssh_signing_keys", "")

		assert.Contains(t, out, "status=503")
		assert.Contains(t, out, gateNamespace+"/nobody")

		out = call(t, "POST", "https://api.github.com/user/ssh_signing_keys", `{"title":"early","key":"ssh-ed25519 AAAA early"}`)
		assert.Contains(t, out, "status=503", "alice has not been authorized yet")
	})

	t.Run("the user approves the code shown in the status and the authorization becomes Ready", func(t *testing.T) {
		c.authorizeThroughDeviceFlow(ctx, t, "alice", userLogin)

		login := c.kubectl(t, "", "get", "githubuserauthorization/alice", "-n", gateNamespace, "-o", "jsonpath={.status.login}")
		assert.Equal(t, userLogin, strings.TrimSpace(login))
	})

	t.Run("a pod adds and deletes a signing key with a user rule and never sees the user token", func(t *testing.T) {
		out := call(t, "POST", "https://api.github.com/user/ssh_signing_keys", `{"title":"e2e","key":"ssh-ed25519 AAAAC3e2e"}`)
		require.Contains(t, out, "status=201", out)
		assert.Contains(t, out, `"title":"e2e"`)

		id := ""

		for line := range strings.SplitSeq(out, "\n") {
			if rest, found := strings.CutPrefix(strings.TrimSpace(line), `{"id":`); found {
				id, _, _ = strings.Cut(rest, ",")
			}
		}

		require.NotEmpty(t, id, "the created key has an id: %s", out)

		out = call(t, "DELETE", "https://api.github.com/user/ssh_signing_keys/"+id, "")
		assert.Contains(t, out, "status=204", out)
	})

	t.Run("operations the rule doesn't list, GraphQL and git never get the user token", func(t *testing.T) {
		// A listed operation succeeds only with the user token: the fake
		// refuses installation tokens on /user paths.
		out := call(t, "GET", "https://api.github.com/user/ssh_signing_keys", "")
		assert.Contains(t, out, "status=503", "the pending authorization's rule still covers this operation")

		out = call(t, "GET", "https://api.github.com/user", "")
		assert.NotContains(t, out, "status=200", "an unlisted operation is not sent as the user")
		assert.NotContains(t, out, `"login":"`+userLogin+`"`)

		out = call(t, "POST", "https://api.github.com/graphql", `{"query":"{ viewer { login } }"}`)
		assert.NotContains(t, out, userLogin, "GraphQL is not sent as the user")

		script := `git -c http.sslCAInfo=/var/run/secrets/githubgate-proxy/ca.crt ls-remote https://github.com/` + owner + "/" + repository + `.git 2>&1; echo "status=$?"`
		out, err := c.exec(ctx, callerNamespace, userCaller, "export GIT_TERMINAL_PROMPT=0\n"+script)
		recordOutput(out)
		require.NoError(t, err, out)
		assert.NotContains(t, out, userLogin)
	})
}
