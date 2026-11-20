package github_test

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/permissions"
	"github.com/solidDoWant/githubgate/test/fakegithub"
)

func isMint(request *http.Request) bool {
	return request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens")
}

// answerMint answers token requests with a fixed status, headers and message.
func answerMint(status int, headers map[string]string, message string) func(http.ResponseWriter, *http.Request) bool {
	return func(writer http.ResponseWriter, request *http.Request) bool {
		if !isMint(request) {
			return false
		}

		for name, value := range headers {
			writer.Header().Set(name, value)
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(map[string]string{"message": message})

		return true
	}
}

// rewriteMint lets the fake mint the token, then changes GitHub's answer.
func rewriteMint(fake *fakegithub.Server, change func(body map[string]any)) func(http.ResponseWriter, *http.Request) bool {
	handler := fake.Handler()

	return func(writer http.ResponseWriter, request *http.Request) bool {
		if !isMint(request) {
			return false
		}

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			panic(err)
		}

		change(body)

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(recorder.Code)
		_ = json.NewEncoder(writer).Encode(body)

		return true
	}
}

func TestMintRequestsExactlyTheResolvedScope(t *testing.T) {
	f := newFixture(t)

	token, err := f.client(t, nil).Mint(t.Context(), github.MintRequest{
		Installation: "githubgate/acme",
		Owner:        owner,
		Repositories: []string{"one", "two"},
		Permissions:  map[string]permissions.Level{"contents": "read", "issues": "write"},
	})
	require.NoError(t, err)

	require.JSONEq(t, `{"repositories": ["one", "two"], "permissions": {"contents": "read", "issues": "write"}}`, string(f.lastBody()))

	assert.NotEmpty(t, token.Token)
	assert.ElementsMatch(t, []string{"one", "two"}, token.Repositories)
	assert.Equal(t, map[string]permissions.Level{"contents": "read", "issues": "write", "metadata": "read"}, token.Permissions)

	mints := f.fake.MintLog()
	require.Len(t, mints, 1)
	assert.Equal(t, []string{"one", "two"}, mints[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "read", "issues": "write", "metadata": "read"}, mints[0].Permissions)
}

func TestMintRefusesWithoutCallingGitHub(t *testing.T) {
	manyRepositories := func(count int) []string {
		names := make([]string, count)
		for index := range names {
			names[index] = "repo" + strconv.Itoa(index)
		}

		return names
	}

	tests := []struct {
		name         string
		repositories []string
		permissions  map[string]permissions.Level
		errorCheck   require.ErrorAssertionFunc
	}{
		{
			name:        "no repositories",
			permissions: map[string]permissions.Level{"contents": "read"},
			errorCheck:  func(t require.TestingT, err error, _ ...any) { require.ErrorIs(t, err, github.ErrEmptyScope) },
		},
		{
			name:         "no permissions",
			repositories: []string{"one"},
			errorCheck:   func(t require.TestingT, err error, _ ...any) { require.ErrorIs(t, err, github.ErrEmptyScope) },
		},
		{
			name:         "more than 500 repositories",
			repositories: manyRepositories(501),
			permissions:  map[string]permissions.Level{"contents": "read"},
			errorCheck: func(t require.TestingT, err error, _ ...any) {
				var failure *github.Error
				require.ErrorAs(t, err, &failure)
				require.Equal(t, github.ReasonInvalidRequest, failure.Reason)
				require.Equal(t, http.StatusBadRequest, failure.HTTPStatus())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			_, err := f.client(t, nil).Mint(t.Context(), github.MintRequest{
				Installation: "githubgate/acme", Owner: owner, Repositories: test.repositories, Permissions: test.permissions,
			})
			test.errorCheck(t, err)
			require.Zero(t, f.count(accessTokens(installationID)))
		})
	}

	t.Run("exactly 500 repositories reach GitHub", func(t *testing.T) {
		f := newFixture(t)

		_, err := f.client(t, nil).Mint(t.Context(), github.MintRequest{
			Installation: "githubgate/acme", Owner: owner, Repositories: manyRepositories(500), Permissions: map[string]permissions.Level{"contents": "read"},
		})
		requireGitHubError(t, err, github.ReasonRefused, "")
		require.Equal(t, 1, f.count(accessTokens(installationID)))
	})
}

func TestMintMapsGitHubAnswers(t *testing.T) {
	tests := []struct {
		name       string
		request    func(request *github.MintRequest)
		override   func(f *fixture) func(http.ResponseWriter, *http.Request) bool
		reason     github.Reason
		status     int
		condition  string
		retryAfter time.Duration
		message    string
	}{
		{
			name:    "an inaccessible repository is refused with GitHub's message",
			request: func(request *github.MintRequest) { request.Repositories = []string{"missing"} },
			reason:  github.ReasonRefused,
			status:  http.StatusForbidden,
			message: "There is at least one repository that does not exist or is not accessible to the parent installation.",
		},
		{
			name: "a permission above the grant is refused with GitHub's message",
			request: func(request *github.MintRequest) {
				request.Permissions = map[string]permissions.Level{"administration": "write"}
			},
			reason:  github.ReasonRefused,
			status:  http.StatusForbidden,
			message: "The permissions requested are not granted to this installation.",
		},
		{
			name: "a server error is unavailable",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return answerMint(http.StatusBadGateway, nil, "Server Error")
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a dropped connection is unavailable",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return func(_ http.ResponseWriter, request *http.Request) bool {
					if isMint(request) {
						panic(http.ErrAbortHandler)
					}

					return false
				}
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a rejected JWT is unavailable and marks the App's Secret invalid",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return answerMint(http.StatusUnauthorized, nil, "A JSON web token could not be decoded")
			},
			reason:    github.ReasonUnavailable,
			status:    http.StatusServiceUnavailable,
			condition: github.ConditionSecretInvalid,
		},
		{
			name: "a suspended installation is refused and marked suspended",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				if err := f.fake.SuspendInstallation(installationID, true); err != nil {
					panic(err)
				}

				return nil
			},
			reason:    github.ReasonRefused,
			status:    http.StatusForbidden,
			condition: github.ConditionInstallationSuspended,
			message:   "This installation has been suspended",
		},
		{
			name: "a 429 rate limit is unavailable and passes Retry-After on",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return answerMint(http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, "Too many requests")
			},
			reason:     github.ReasonUnavailable,
			status:     http.StatusServiceUnavailable,
			retryAfter: 30 * time.Second,
		},
		{
			name: "a 403 secondary rate limit is unavailable and passes Retry-After on",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return answerMint(http.StatusForbidden, map[string]string{"Retry-After": "45"}, "You have exceeded a secondary rate limit.")
			},
			reason:     github.ReasonUnavailable,
			status:     http.StatusServiceUnavailable,
			retryAfter: 45 * time.Second,
		},
		{
			name: "a 403 secondary rate limit named by its documentation link is unavailable",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return func(writer http.ResponseWriter, request *http.Request) bool {
					if !isMint(request) {
						return false
					}

					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(writer).Encode(map[string]string{
						"message":           "You have exceeded a secondary rate limit.",
						"documentation_url": "https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits",
					})

					return true
				}
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a 403 primary rate limit is unavailable",
			override: func(*fixture) func(http.ResponseWriter, *http.Request) bool {
				return answerMint(http.StatusForbidden, map[string]string{
					"X-RateLimit-Remaining": "0",
					"X-RateLimit-Limit":     "5000",
					"X-RateLimit-Reset":     strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
				}, "API rate limit exceeded")
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a token for an extra repository is discarded",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				return rewriteMint(f.fake, func(body map[string]any) {
					body["repositories"] = append(body["repositories"].([]any), map[string]any{"name": "two"})
				})
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a token for every repository is discarded",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				return rewriteMint(f.fake, func(body map[string]any) { delete(body, "repositories") })
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a token with a higher level is discarded",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				return rewriteMint(f.fake, func(body map[string]any) {
					body["permissions"].(map[string]any)["contents"] = "write"
				})
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a token with an extra permission is discarded",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				return rewriteMint(f.fake, func(body map[string]any) {
					body["permissions"].(map[string]any)["issues"] = "read"
				})
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "a token with metadata above read is discarded",
			override: func(f *fixture) func(http.ResponseWriter, *http.Request) bool {
				return rewriteMint(f.fake, func(body map[string]any) {
					body["permissions"].(map[string]any)["metadata"] = "write"
				})
			},
			reason: github.ReasonUnavailable,
			status: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)

			if test.override != nil {
				f.setOverride(test.override(f))
			}

			request := readRequest()
			if test.request != nil {
				test.request(&request)
			}

			token, err := f.client(t, nil).Mint(t.Context(), request)
			require.Nil(t, token)

			requireGitHubError(t, err, test.reason, test.condition)

			var failure *github.Error
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, test.status, failure.HTTPStatus())
			assert.Equal(t, test.retryAfter, failure.RetryAfter)

			if test.message != "" {
				assert.Contains(t, failure.Message, test.message)
			}
		})
	}
}

func TestMintAcceptsGitHubsAnswerIgnoringCase(t *testing.T) {
	f := newFixture(t)
	f.setOverride(rewriteMint(f.fake, func(body map[string]any) {
		body["repositories"] = []any{map[string]any{"name": "ONE"}}
	}))

	token, err := f.client(t, nil).Mint(t.Context(), readRequest())
	require.NoError(t, err)
	assert.Equal(t, []string{"ONE"}, token.Repositories)
}

func TestMintUpdatesTheRateLimitGaugeAtMostOnceAMinute(t *testing.T) {
	f := newFixture(t)
	metrics := github.NewMetrics()
	client := f.client(t, metrics)
	gauge := metrics.Collectors()[0]

	other := readRequest()
	other.Installation = "team/acme"

	steps := []struct {
		advance time.Duration
		request github.MintRequest
		probes  int
	}{
		{probes: 1, request: readRequest()},
		{probes: 1, request: readRequest()},
		{advance: time.Minute - time.Second, probes: 1, request: readRequest()},
		{probes: 2, request: other},
		{advance: time.Second, probes: 3, request: readRequest()},
	}

	for index, step := range steps {
		f.clock.Advance(step.advance)

		_, err := client.Mint(t.Context(), step.request)
		require.NoError(t, err)
		require.Equal(t, step.probes, f.count("GET /rate_limit"), fmt.Sprintf("after mint %d", index+1))
	}

	expected := `
# HELP githubgate_github_rate_limit_remaining Requests left in the GitHub rate limit of an installation, read after a token is minted.
# TYPE githubgate_github_rate_limit_remaining gauge
githubgate_github_rate_limit_remaining{installation="githubgate/acme"} 4999
githubgate_github_rate_limit_remaining{installation="team/acme"} 4999
`
	require.NoError(t, testutil.CollectAndCompare(gauge, strings.NewReader(expected)))
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	f := newFixture(t)
	registry := f.registry(github.NewMetrics())

	var texts []string

	record := func(err error) {
		require.Error(t, err)
		texts = append(texts, err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
	}

	mint := func(credentials github.Credentials, request github.MintRequest) {
		client, err := registry.Client(appName, credentials)
		if err != nil {
			record(err)

			return
		}

		_, err = client.Mint(t.Context(), request)
		record(err)
	}

	good := github.Credentials{AppID: appID, PrivateKey: f.key, Version: "good"}
	wrongKey := otherKey(t)

	// A missing key, then a key GitHub rejects.
	mint(github.Credentials{AppID: appID, Version: "missing"}, readRequest())
	mint(github.Credentials{AppID: appID, PrivateKey: wrongKey, Version: "wrong"}, readRequest())

	// GitHub refuses the scope.
	refused := readRequest()
	refused.Repositories = []string{"missing"}
	mint(good, refused)

	// GitHub mints a token for more than was asked; it is discarded.
	f.setOverride(rewriteMint(f.fake, func(body map[string]any) {
		body["permissions"].(map[string]any)["contents"] = "write"
	}))
	mint(good, readRequest())

	// GitHub fails.
	f.setOverride(answerMint(http.StatusInternalServerError, nil, "Server Error"))
	mint(good, readRequest())
	f.setOverride(nil)

	// The installation is suspended, then gone.
	require.NoError(t, f.fake.SuspendInstallation(installationID, true))
	mint(good, readRequest())
	f.fake.DeleteInstallation(installationID)
	f.clock.Advance(time.Minute)
	mint(good, readRequest())

	secrets := []string{f.key.N.String(), f.key.D.String(), wrongKey.N.String(), wrongKey.D.String()}

	for _, key := range []*rsa.PrivateKey{f.key, wrongKey} {
		for _, line := range strings.Split(string(keyPEM(key)), "\n") {
			if len(line) > 16 && !strings.HasPrefix(line, "-----") {
				secrets = append(secrets, line)
			}
		}
	}

	jwts := f.fake.JWTs()
	require.NotEmpty(t, jwts)
	secrets = append(secrets, jwts...)

	for _, jwt := range jwts {
		// The signature alone would identify the JWT.
		secrets = append(secrets, jwt[strings.LastIndex(jwt, ".")+1:])
	}

	tokens := f.fake.Tokens()
	require.NotEmpty(t, tokens, "a discarded token must be part of the search")

	for _, token := range tokens {
		secrets = append(secrets, token.Token, strings.TrimPrefix(token.Token, "ghs_"))
	}

	for _, text := range texts {
		for _, secret := range secrets {
			require.NotContains(t, text, secret)
		}
	}
}
