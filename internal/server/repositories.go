package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

const (
	// repositoryNamesTTL is how long a repository ID's name is kept. A
	// renamed or moved repository is named again after it.
	repositoryNamesTTL = 10 * time.Minute
	// unreachedRepositoriesTTL is how long GitHub's answer that an
	// installation doesn't reach a repository ID is kept. It spares GitHub a
	// lookup, paid from the App's rate limit, for each refused request, and
	// is short so a repository newly added to the installation is soon
	// named.
	unreachedRepositoriesTTL = time.Minute
	// maxRepositoryNames bounds the entries each cache keeps at once.
	maxRepositoryNames = 4096
)

type repositoryNameKey struct {
	installation string
	id           int64
}

type repositoryName struct {
	owner, name string
	expires     time.Time
}

// repositoryNames caches the repository names of IDs per installation, or,
// for the unreached cache, the IDs an installation doesn't reach.
type repositoryNames struct {
	mu      sync.Mutex
	entries map[repositoryNameKey]repositoryName
}

func (c *repositoryNames) get(key repositoryNameKey, now time.Time) (repositoryName, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if ok && !now.Before(entry.expires) {
		delete(c.entries, key)

		return repositoryName{}, false
	}

	return entry, ok
}

func (c *repositoryNames) put(key repositoryNameKey, entry repositoryName, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = map[repositoryNameKey]repositoryName{}
	}

	if len(c.entries) >= maxRepositoryNames {
		for existing, kept := range c.entries {
			if !now.Before(kept.expires) {
				delete(c.entries, existing)
			}
		}
	}

	for existing := range c.entries {
		if len(c.entries) < maxRepositoryNames {
			break
		}

		delete(c.entries, existing)
	}

	c.entries[key] = entry
}

// forget drops the entries of id in every installation.
func (c *repositoryNames) forget(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.entries {
		if key.id == id {
			delete(c.entries, key)
		}
	}
}

// RepositoryName names the repository with id for the proxy, through the
// first of installations that reaches it. Names are cached per installation,
// so a caller paging through a list costs one GitHub call per repository.
func (s *Server) RepositoryName(ctx context.Context, installations []*model.Installation, id int64) (string, string, error) {
	var failure error

	for _, installation := range installations {
		key := repositoryNameKey{installation: installation.String(), id: id}

		if entry, ok := s.repositoryNames.get(key, s.options.Now()); ok {
			return entry.owner, entry.name, nil
		}

		if _, ok := s.unreachedRepositories.get(key, s.options.Now()); ok {
			continue
		}

		owner, name, err := s.lookUpRepositoryName(ctx, installation, id)

		switch {
		case err == nil:
			s.repositoryNames.put(key, repositoryName{owner: owner, name: name, expires: s.options.Now().Add(repositoryNamesTTL)}, s.options.Now())

			return owner, name, nil
		case errors.Is(err, github.ErrRepositoryNotReached):
			// Only GitHub's own answer is kept: a rate limit or any other
			// failure is asked again next time, and fails closed now.
			if installation.App != nil {
				s.unreachedRepositories.put(key, repositoryName{expires: s.options.Now().Add(unreachedRepositoriesTTL)}, s.options.Now())
			}
		default:
			failure = err
		}
	}

	if failure != nil {
		return "", "", failure
	}

	return "", "", proxy.ErrRepositoryNotFound
}

// RepositoryNameNow is RepositoryName asked of GitHub: the cached names of
// id are dropped first, and the answer is cached again.
func (s *Server) RepositoryNameNow(ctx context.Context, installations []*model.Installation, id int64) (string, string, error) {
	s.repositoryNames.forget(id)

	return s.RepositoryName(ctx, installations, id)
}

func (s *Server) lookUpRepositoryName(ctx context.Context, installation *model.Installation, id int64) (string, string, error) {
	app := installation.App
	if app == nil {
		return "", "", github.ErrRepositoryNotReached
	}

	client, err := s.options.GitHub.Client(app.NamespacedName, github.Credentials{
		AppID: app.AppID, PrivateKey: app.PrivateKey(), Version: app.KeyVersion,
	})
	if err != nil {
		return "", "", err
	}

	started := time.Now()
	defer s.observeGitHub("mint", started)

	return client.RepositoryName(ctx, installation.Owner, id)
}
