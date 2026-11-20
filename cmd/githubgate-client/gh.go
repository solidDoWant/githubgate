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

// runGH gets a fresh token and runs gh with GH_TOKEN set.
func runGH(ctx context.Context, env *environment, args []string) error {
	var repo string

	for len(args) > 0 && args[0] != "--" {
		switch {
		case args[0] == "--repo" && len(args) > 1:
			repo, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--repo="):
			repo, args = strings.TrimPrefix(args[0], "--repo="), args[1:]
		default:
			return errors.New("usage: gh [--repo owner/name] -- <gh args>")
		}
	}

	if len(args) > 0 {
		args = args[1:]
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

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = env.stdin
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stderr

	cmd.Env = append(os.Environ(), "GH_TOKEN="+response.Token)

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
