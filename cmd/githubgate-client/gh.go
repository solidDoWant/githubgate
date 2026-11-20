package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// runGH gets a fresh token and runs gh with GH_TOKEN set. With --proxy the
// token is the ServiceAccount token, and gh trusts only the proxy CA.
func runGH(ctx context.Context, env *environment, args []string) error {
	var repo string

	proxy := false

	for len(args) > 0 && args[0] != "--" {
		switch {
		case args[0] == "--proxy":
			proxy, args = true, args[1:]
		case args[0] == "--repo" && len(args) > 1:
			repo, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--repo="):
			repo, args = strings.TrimPrefix(args[0], "--repo="), args[1:]
		default:
			return errors.New("usage: gh [--repo owner/name | --proxy] -- <gh args>")
		}
	}

	if len(args) > 0 {
		args = args[1:]
	}

	if proxy {
		if repo != "" {
			return errors.New("--repo has no effect with --proxy: the proxy decides per request")
		}

		return runProxiedGH(ctx, env, args)
	}

	if repo == "" {
		repo = originRepository(ctx)
	}

	request := tokenRequest{}

	if repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("--repo %q must look like owner/name", repo)
		}

		request.Owner = owner
		request.Repositories = []string{name}
	}

	response, err := requestToken(ctx, env, request)
	if err != nil {
		return err
	}

	return execGH(ctx, env, args, "GH_TOKEN="+response.Token)
}

// runProxiedGH runs gh with the ServiceAccount token and the proxy CA as its
// only trusted CA. gh is a Go program, and Go loads the system certificate
// directories even when SSL_CERT_FILE is set, so SSL_CERT_DIR points at an
// empty directory.
func runProxiedGH(ctx context.Context, env *environment, args []string) error {
	caFile, _, err := proxyCA(env)
	if err != nil {
		return err
	}

	token, err := serviceAccountToken(env)
	if err != nil {
		return err
	}

	emptyDir, err := os.MkdirTemp("", "githubgate-no-certs-")
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(emptyDir) }()

	return execGH(ctx, env, args, "GH_TOKEN="+token, "SSL_CERT_FILE="+caFile, "SSL_CERT_DIR="+emptyDir)
}

func execGH(ctx context.Context, env *environment, args []string, vars ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = env.stdin
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stderr

	cmd.Env = append(os.Environ(), vars...)

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError(exit.ExitCode())
		}

		return fmt.Errorf("run gh: %w", err)
	}

	return nil
}

// originRepository returns owner/name from the origin remote when it points at
// github.com, else an empty string.
func originRepository(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}

	return parseGitHubRemote(strings.TrimSpace(string(out)))
}

func parseGitHubRemote(remote string) string {
	var path string

	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		path = strings.TrimPrefix(remote, "git@github.com:")
	default:
		parsed, err := url.Parse(remote)
		if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
			return ""
		}

		path = parsed.Path
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")

	owner, name, ok := strings.Cut(path, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}

	return owner + "/" + name
}
