package server

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/permissions"
)

const (
	// tokenCacheSize bounds the number of cached tokens per replica.
	tokenCacheSize = 10_000
	// tokenCacheTTL is GitHub's fixed token lifetime: no entry can be useful longer.
	tokenCacheTTL = time.Hour
	// minRemaining is the least lifetime a cached token must have to be handed out.
	minRemaining = 10 * time.Minute
	// mintTimeout bounds a mint run on behalf of every caller waiting for it.
	mintTimeout = 10 * time.Second
)

// tokenKey identifies the tokens one caller may share: the values a request
// resolved to, never an object UID, so a changed owner or appRef can't hand
// out a token from the old installation.
type tokenKey struct {
	namespace      string
	serviceAccount string
	appID          int64
	installationID int64
	repositories   string
	permissions    string
}

func newTokenKey(namespace, serviceAccount string, appID, installationID int64, repositories []string, set map[string]permissions.Level) tokenKey {
	names := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		names = append(names, strings.ToLower(repository))
	}

	sort.Strings(names)

	levels := make([]string, 0, len(set))
	for name, level := range set {
		levels = append(levels, name+":"+string(level))
	}

	sort.Strings(levels)

	return tokenKey{
		namespace:      namespace,
		serviceAccount: serviceAccount,
		appID:          appID,
		installationID: installationID,
		repositories:   strings.Join(names, ","),
		permissions:    strings.Join(levels, ","),
	}
}

func (k tokenKey) String() string {
	return strings.Join([]string{
		k.namespace, k.serviceAccount, strconv.FormatInt(k.appID, 10), strconv.FormatInt(k.installationID, 10),
		k.repositories, k.permissions,
	}, "\x00")
}

// tokenCache holds minted tokens in memory and de-duplicates concurrent mints
// of the same key.
type tokenCache struct {
	tokens *expirable.LRU[tokenKey, *github.Token]
	group  singleflight.Group
	now    func() time.Time
}

func newTokenCache(now func() time.Time) *tokenCache {
	return &tokenCache{tokens: expirable.NewLRU[tokenKey, *github.Token](tokenCacheSize, nil, tokenCacheTTL), now: now}
}

// get returns a cached token for key with at least minRemaining left.
func (c *tokenCache) get(key tokenKey) (*github.Token, bool) {
	token, ok := c.tokens.Get(key)
	if !ok {
		return nil, false
	}

	if token.ExpiresAt.Sub(c.now()) < minRemaining {
		c.tokens.Remove(key)

		return nil, false
	}

	return token, true
}

// mint runs one mint per key at a time. The mint runs with a context detached
// from the caller's, so one caller giving up doesn't fail the others waiting.
func (c *tokenCache) mint(ctx context.Context, key tokenKey, mint func(context.Context) (*github.Token, error)) (*github.Token, error) {
	result, err, _ := c.group.Do(key.String(), func() (any, error) {
		if token, ok := c.get(key); ok {
			return token, nil
		}

		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
		defer cancel()

		token, err := mint(detached)
		if err != nil {
			return nil, err
		}

		c.tokens.Add(key, token)

		return token, nil
	})
	if err != nil {
		return nil, err
	}

	return result.(*github.Token), nil
}
