//go:build integration

package controller_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/githubgate/internal/controller"
)

// otherHost is a server that is not GitHub; it records every request it
// gets, with its form body.
type otherHost struct {
	url string

	mu       sync.Mutex
	received []string
}

func startOtherHost(t *testing.T) *otherHost {
	t.Helper()

	host := &otherHost{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)

		host.mu.Lock()
		host.received = append(host.received, request.Method+" "+request.URL.Path+" "+string(body))
		host.mu.Unlock()

		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	host.url = server.URL

	return host
}

func (h *otherHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.received...)
}

// redirectOAuth answers the OAuth requests that match with a redirect of
// status to host until the test ends, and counts them.
func (e *env) redirectOAuth(t *testing.T, matches func(*http.Request) bool, status int, host *otherHost) *atomic.Int64 {
	t.Helper()

	var redirected atomic.Int64

	intercept := func(writer http.ResponseWriter, request *http.Request) bool {
		if !matches(request) {
			return false
		}

		redirected.Add(1)
		http.Redirect(writer, request, host.url+"/steal", status)

		return true
	}
	e.intercept.Store(&intercept)
	t.Cleanup(func() { e.intercept.Store(nil) })

	return &redirected
}

func isDeviceCodeRequest(request *http.Request) bool {
	return request.Method == http.MethodPost && request.URL.Path == "/login/device/code"
}

func isDevicePoll(request *http.Request) bool {
	if request.Method != http.MethodPost || request.URL.Path != "/login/oauth/access_token" {
		return false
	}

	_ = request.ParseForm()

	return request.PostForm.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code"
}

func TestOAuthCallsDoNotFollowRedirects(t *testing.T) {
	t.Parallel()

	e := startDeviceFlowEnv(t)

	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("a refresh grant answered with HTTP %d sends nothing to the other host and is a failed refresh", status), func(t *testing.T) {
			host := startOtherHost(t)
			redirected := e.redirectOAuth(t, isRefreshGrant, status, host)

			object, first := e.authorizeShortLived(t, fmt.Sprintf("redirect-refresh-%d", status))

			require.Eventually(t, func() bool {
				condition := readyCondition(t, e, object)

				return redirected.Load() >= 2 && condition != nil && condition.Status == metav1.ConditionTrue &&
					strings.Contains(condition.Message, "RefreshFailed")
			}, 30*time.Second, 50*time.Millisecond, "waiting for the redirected refreshes")

			assert.Empty(t, host.requests())
			assert.Contains(t, e.replicaTokens(t, object), first)
		})

		t.Run(fmt.Sprintf("a device code request answered with HTTP %d sends nothing to the other host and ends the flow", status), func(t *testing.T) {
			host := startOtherHost(t)
			redirected := e.redirectOAuth(t, isDeviceCodeRequest, status, host)

			object := deviceAuthorization(fmt.Sprintf("redirect-code-%d", status))
			e.create(t, object)

			e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonFlowLost)
			requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonFlowLost)
			assert.Positive(t, redirected.Load())
			assert.Empty(t, host.requests())
			e.requireNoTokenSecret(t, object.Name)
		})

		t.Run(fmt.Sprintf("a device flow poll answered with HTTP %d sends nothing to the other host and ends the flow", status), func(t *testing.T) {
			host := startOtherHost(t)
			redirected := e.redirectOAuth(t, isDevicePoll, status, host)

			object := deviceAuthorization(fmt.Sprintf("redirect-poll-%d", status))
			e.create(t, object)

			e.requireReady(t, object, func() []metav1.Condition { return object.Status.Conditions }, metav1.ConditionFalse, controller.ReasonFlowLost)
			requireAuthorizationRequired(t, object, metav1.ConditionTrue, controller.ReasonFlowLost)
			assert.Nil(t, object.Status.Authorization)
			assert.Positive(t, redirected.Load())
			assert.Empty(t, host.requests())
			e.requireNoTokenSecret(t, object.Name)
		})
	}
}
