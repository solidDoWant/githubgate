package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v93/github"

	"github.com/solidDoWant/githubgate/internal/permissions"
)

// MintRequest names the installation and exactly the repositories and
// permissions the token gets.
type MintRequest struct {
	// Installation is the GitHubInstallation's namespace/name, the metrics label.
	Installation string
	// Owner is the GitHubInstallation's spec.owner.
	Owner        string
	Repositories []string
	Permissions  map[string]permissions.Level
}

// Token is an installation token with the repositories and permissions
// GitHub gave it.
type Token struct {
	Token        string
	ExpiresAt    time.Time
	Repositories []string
	// RepositoryIDs are the IDs of Repositories. GitHub binds a token to
	// repository IDs, so it keeps reaching a renamed repository and never
	// reaches a new one that takes the old name.
	RepositoryIDs []int64
	Permissions   map[string]permissions.Level
}

// accessTokenBody is built by hand rather than with go-github's
// InstallationTokenOptions: GitHub widens a token to every repository or the
// whole grant when a field is missing, and those options drop empty or
// unknown values.
type accessTokenBody struct {
	Repositories []string                     `json:"repositories"`
	Permissions  map[string]permissions.Level `json:"permissions"`
}

type accessTokenResponse struct {
	Token        string            `json:"token"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"repositories"`
}

// Mint creates an installation token for exactly the requested repositories
// and permissions. It refuses an empty set without calling GitHub, and
// discards a token whose repositories or permissions differ from the request.
// A 404 refreshes the installation id and retries once.
func (c *AppClient) Mint(ctx context.Context, request MintRequest) (*Token, error) {
	if len(request.Repositories) == 0 || len(request.Permissions) == 0 {
		return nil, ErrEmptyScope
	}

	if len(request.Repositories) > maxRepositories {
		return nil, &Error{Reason: ReasonInvalidRequest, Message: fmt.Sprintf("a token can name at most %d repositories", maxRepositories)}
	}

	id, err := c.InstallationID(ctx, request.Owner)
	if err != nil {
		return nil, err
	}

	token, err := c.mint(ctx, id, request)

	var failure *Error
	if errors.As(err, &failure) && failure.Condition == ConditionInstallationNotFound {
		if id, err = c.installationID(ctx, request.Owner, true); err != nil {
			return nil, err
		}

		token, err = c.mint(ctx, id, request)
	}

	if err != nil {
		return nil, err
	}

	c.probeRateLimit(ctx, request.Installation, token.Token)

	return token, nil
}

func (c *AppClient) mint(ctx context.Context, id int64, request MintRequest) (*Token, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	body := accessTokenBody{Repositories: request.Repositories, Permissions: request.Permissions}

	httpRequest, err := c.api.NewRequest(ctx, http.MethodPost, fmt.Sprintf("app/installations/%d/access_tokens", id), body)
	if err != nil {
		return nil, unavailable("building the token request failed")
	}

	var response accessTokenResponse
	if _, err := c.api.Do(httpRequest, &response); err != nil {
		return nil, classify(err, c.now())
	}

	token, ok := checkToken(response, request)
	if !ok {
		return nil, unavailable("GitHub returned a token for other repositories or permissions than requested; it was discarded")
	}

	return token, nil
}

// checkToken accepts GitHub's answer only when it names exactly the requested
// repositories (case-insensitively) and no permission above the request,
// except metadata: read, which every installation token has.
func checkToken(response accessTokenResponse, request MintRequest) (*Token, bool) {
	if response.Token == "" {
		return nil, false
	}

	want := make([]string, 0, len(request.Repositories))
	for _, name := range request.Repositories {
		want = append(want, strings.ToLower(name))
	}

	got := make([]string, 0, len(response.Repositories))
	names := make([]string, 0, len(response.Repositories))
	ids := make([]int64, 0, len(response.Repositories))

	for _, repository := range response.Repositories {
		got = append(got, strings.ToLower(repository.Name))
		names = append(names, repository.Name)
		ids = append(ids, repository.ID)
	}

	slices.Sort(want)
	slices.Sort(got)

	if !slices.Equal(slices.Compact(want), got) {
		return nil, false
	}

	granted := make(map[string]permissions.Level, len(response.Permissions))

	for name, raw := range response.Permissions {
		level := permissions.Level(raw)
		requested, ok := request.Permissions[name]

		switch {
		case ok && requested.AtLeast(level):
		case name == "metadata" && level == permissions.LevelRead:
		default:
			return nil, false
		}

		granted[name] = level
	}

	return &Token{Token: response.Token, ExpiresAt: response.ExpiresAt, Repositories: names, RepositoryIDs: ids, Permissions: granted}, true
}

// probeRateLimit reads the installation's remaining rate limit with the new
// token, at most once per rateLimitProbeInterval per installation. Reading
// the rate limit does not count against it. A failed probe only leaves the
// gauge unchanged.
func (c *AppClient) probeRateLimit(ctx context.Context, installation, token string) {
	metrics := c.registry.options.Metrics
	if metrics == nil {
		return
	}

	registry := c.registry
	now := c.now()

	registry.mu.Lock()
	last, probed := registry.probes[installation]

	if probed && now.Sub(last) < rateLimitProbeInterval {
		registry.mu.Unlock()

		return
	}

	registry.probes[installation] = now
	registry.mu.Unlock()

	api, err := gogithub.NewClient(
		gogithub.WithTransport(registry.options.Transport),
		gogithub.WithAuthToken(token),
		gogithub.WithURLs(&registry.options.BaseURL, nil),
	)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	limits, _, err := api.RateLimit.Get(ctx)
	if err != nil || limits.GetCore() == nil {
		return
	}

	metrics.rateLimitRemaining.WithLabelValues(installation).Set(float64(limits.GetCore().Remaining))
}
