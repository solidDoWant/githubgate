// Package github talks to GitHub as a GitHub App: it keeps one App client per
// GitHubApp, looks up installation ids and mints installation tokens limited
// to exactly the repositories and permissions it is given.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v93/github"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// callTimeout bounds every call to GitHub.
	callTimeout = 10 * time.Second
	// installationTTL is how long a looked-up installation id is used.
	installationTTL = 10 * time.Minute
	// minRefreshInterval is the shortest time between two listings of one App's installations.
	minRefreshInterval = 30 * time.Second
	// rateLimitProbeInterval is the shortest time between two rate-limit probes of one installation.
	rateLimitProbeInterval = time.Minute
	// defaultBaseURL is GitHub's REST API.
	defaultBaseURL = "https://api.github.com/"
)

// Options configure a Registry.
type Options struct {
	// BaseURL is GitHub's REST API root; api.github.com when empty.
	BaseURL string
	// Transport carries every request; http.DefaultTransport when nil.
	Transport http.RoundTripper
	// Metrics receives the rate-limit gauge; nothing is recorded when nil.
	Metrics *Metrics
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Credentials are a GitHubApp's App ID and parsed private key, read from the
// GitHubApp and its Secrets. Version changes whenever either may have changed
// (the Secrets' resourceVersions).
type Credentials struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
	Version    string
}

// Registry holds one App client per GitHubApp and the installation ids each
// App's installations have. It is safe for concurrent use.
type Registry struct {
	options Options

	mu            sync.Mutex
	clients       map[types.NamespacedName]*AppClient
	installations map[int64]*installationCache
	probes        map[string]time.Time
}

// NewRegistry returns an empty Registry.
func NewRegistry(options Options) *Registry {
	if options.BaseURL == "" {
		options.BaseURL = defaultBaseURL
	}

	if !strings.HasSuffix(options.BaseURL, "/") {
		options.BaseURL += "/"
	}

	if options.Transport == nil {
		options.Transport = http.DefaultTransport
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	return &Registry{
		options:       options,
		clients:       map[types.NamespacedName]*AppClient{},
		installations: map[int64]*installationCache{},
		probes:        map[string]time.Time{},
	}
}

// Client returns the App client of a GitHubApp, building a new one when the
// App ID, the version or the private key differ from the ones the current
// client was built with, so a changed Secret takes effect on the next call.
func (r *Registry) Client(app types.NamespacedName, credentials Credentials) (*AppClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if current, ok := r.clients[app]; ok && current.appID == credentials.AppID && current.version == credentials.Version &&
		credentials.PrivateKey != nil && current.privateKey.Equal(credentials.PrivateKey) {
		return current, nil
	}

	if credentials.AppID <= 0 {
		return nil, &Error{Reason: ReasonUnavailable, Message: "the App ID is not set", Condition: ConditionSecretInvalid}
	}

	if credentials.PrivateKey == nil {
		return nil, &Error{Reason: ReasonUnavailable, Message: "the App's private key is not set", Condition: ConditionSecretInvalid}
	}

	transport := ghinstallation.NewAppsTransportFromPrivateKey(r.options.Transport, credentials.AppID, credentials.PrivateKey)
	transport.BaseURL = strings.TrimSuffix(r.options.BaseURL, "/")

	api, err := gogithub.NewClient(gogithub.WithTransport(transport), gogithub.WithURLs(&r.options.BaseURL, nil))
	if err != nil {
		return nil, fmt.Errorf("building the GitHub client: %w", err)
	}

	cache, ok := r.installations[credentials.AppID]
	if !ok {
		cache = &installationCache{}
		r.installations[credentials.AppID] = cache
	}

	// New credentials may succeed where the last listing failed, so a failed
	// listing's limit does not carry over to them.
	cache.mu.Lock()
	if cache.failure != nil {
		cache.attemptedAt = time.Time{}
		cache.failure = nil
	}
	cache.mu.Unlock()

	client := &AppClient{
		registry:      r,
		appID:         credentials.AppID,
		version:       credentials.Version,
		privateKey:    credentials.PrivateKey,
		api:           api,
		installations: cache,
	}
	r.clients[app] = client

	return client, nil
}

// AppClient calls GitHub as one App.
type AppClient struct {
	registry      *Registry
	appID         int64
	version       string
	privateKey    *rsa.PrivateKey
	api           *gogithub.Client
	installations *installationCache
}

// installationCache maps the lower-cased owners of one App's installations to
// their ids, from the last listing.
type installationCache struct {
	group singleflight.Group

	mu       sync.Mutex
	byOwner  map[string]InstallationInfo
	listedAt time.Time
	// attemptedAt and failure record the last listing, whether or not it
	// worked, so failed listings are limited like successful ones.
	attemptedAt time.Time
	failure     error
}

func (c *AppClient) now() time.Time { return c.registry.options.Now() }

// InstallationID returns the id of the App's installation on owner, compared
// case-insensitively. Ids come from listing the App's installations, at most
// installationTTL old.
func (c *AppClient) InstallationID(ctx context.Context, owner string) (int64, error) {
	return c.installationID(ctx, owner, false)
}

func (c *AppClient) installationID(ctx context.Context, owner string, refresh bool) (int64, error) {
	info, err := c.installation(ctx, owner, refresh)

	return info.ID, err
}

// InstallationInfo is what GitHub says about one installation of the App.
type InstallationInfo struct {
	ID int64
	// Permissions is the installation's grant.
	Permissions map[string]string
	Suspended   bool
}

// Installation returns the App's installation on owner, compared
// case-insensitively, from the same listing InstallationID uses.
func (c *AppClient) Installation(ctx context.Context, owner string) (InstallationInfo, error) {
	return c.installation(ctx, owner, false)
}

// App returns the App's slug, as GitHub reports it.
func (c *AppClient) App(ctx context.Context) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	app, _, err := c.api.Apps.Get(callCtx, "")
	if err != nil {
		return "", classify(err, c.now())
	}

	return app.GetSlug(), nil
}

func (c *AppClient) installation(ctx context.Context, owner string, refresh bool) (InstallationInfo, error) {
	key := strings.ToLower(owner)
	cache := c.installations

	cache.mu.Lock()
	info, found := cache.byOwner[key]
	listed := cache.byOwner != nil
	age := c.now().Sub(cache.listedAt)
	attempted := !cache.attemptedAt.IsZero()
	sinceAttempt := c.now().Sub(cache.attemptedAt)
	cache.mu.Unlock()

	if found && listed && age < installationTTL && !refresh {
		return info, nil
	}

	// Listings are shared by concurrent callers and limited to one per
	// minRefreshInterval, failed ones too; inside it the last listing answers.
	if !attempted || sinceAttempt >= minRefreshInterval {
		_, err, _ := cache.group.Do("list", func() (any, error) {
			return nil, c.listInstallations(context.WithoutCancel(ctx))
		})
		if err != nil {
			return InstallationInfo{}, err
		}
	}

	cache.mu.Lock()
	info, found = cache.byOwner[key]
	failure := cache.failure
	cache.mu.Unlock()

	if failure != nil {
		return InstallationInfo{}, failure
	}

	if !found {
		return InstallationInfo{}, &Error{Reason: ReasonUnavailable, Message: "the App has no installation on " + owner, Condition: ConditionInstallationNotFound}
	}

	return info, nil
}

func (c *AppClient) listInstallations(ctx context.Context) error {
	cache := c.installations

	cache.mu.Lock()
	recent := !cache.attemptedAt.IsZero() && c.now().Sub(cache.attemptedAt) < minRefreshInterval
	cache.mu.Unlock()

	if recent {
		return nil
	}

	byOwner := map[string]InstallationInfo{}
	options := &gogithub.ListOptions{PerPage: 100}

	for {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		page, response, err := c.api.Apps.ListInstallations(callCtx, options)

		cancel()

		if err != nil {
			failure := classify(err, c.now())

			cache.mu.Lock()
			cache.attemptedAt = c.now()
			cache.failure = failure
			cache.mu.Unlock()

			return failure
		}

		for _, installation := range page {
			if login := installation.GetAccount().GetLogin(); login != "" {
				info := InstallationInfo{ID: installation.GetID(), Suspended: installation.SuspendedAt != nil}
				if granted := installation.GetPermissions(); granted != nil {
					info.Permissions = grantedPermissions(granted)
				}

				byOwner[strings.ToLower(login)] = info
			}
		}

		if response.NextPage == 0 {
			break
		}

		options.Page = response.NextPage
	}

	cache.mu.Lock()
	cache.byOwner = byOwner
	cache.listedAt = c.now()
	cache.attemptedAt = cache.listedAt
	cache.failure = nil
	cache.mu.Unlock()

	return nil
}

// grantedPermissions flattens go-github's permissions struct to a map of
// permission name to level through its JSON form, so names follow GitHub's.
func grantedPermissions(granted *gogithub.InstallationPermissions) map[string]string {
	raw, err := json.Marshal(granted)
	if err != nil {
		return nil
	}

	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}
