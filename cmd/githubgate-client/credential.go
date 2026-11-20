package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
)

// runGitCredential implements the git credential helper protocol for
// https://github.com. store and erase do nothing, because tokens are never
// kept. With --proxy it answers the ServiceAccount token for the proxy instead
// of asking githubgate for an installation token.
func runGitCredential(ctx context.Context, env *environment, args []string) error {
	proxy := len(args) > 0 && args[0] == "--proxy"
	if proxy {
		args = args[1:]
	}

	if len(args) != 1 {
		return errors.New("usage: git-credential [--proxy] get|store|erase")
	}

	switch args[0] {
	case "store", "erase":
		return nil
	case "get":
	default:
		return fmt.Errorf("unknown git-credential operation %q", args[0])
	}

	input := map[string]string{}
	scanner := bufio.NewScanner(env.stdin)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}

		if key, value, ok := strings.Cut(line, "="); ok {
			input[key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Other hosts and protocols are left to the next helper. In proxy mode
	// this also keeps the ServiceAccount token to the one host git reaches
	// through the proxy.
	if input["protocol"] != "https" || input["host"] != "github.com" {
		return nil
	}

	if proxy {
		token, err := serviceAccountToken(env)
		if err != nil {
			return refuse(env, err.Error())
		}

		_, err = fmt.Fprintf(env.stdout, "username=x-access-token\npassword=%s\n", token)

		return err
	}

	request := tokenRequest{}

	if path := strings.Trim(input["path"], "/"); path != "" {
		owner, name, ok := strings.Cut(path, "/")
		name = strings.TrimSuffix(name, ".git")

		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return refuse(env, fmt.Sprintf("cannot find a repository in the path %q", path))
		}

		request.Owner = owner
		request.Repositories = []string{name}
	}

	response, err := requestToken(ctx, env, request)
	if err != nil {
		return refuse(env, err.Error())
	}

	_, err = fmt.Fprintf(env.stdout, "username=x-access-token\npassword=%s\npassword_expiry_utc=%d\n",
		response.Token, response.ExpiresAt.Unix())

	return err
}

// refuse tells git to stop (quit=1) so it does not ask later helpers or
// GIT_ASKPASS, and prints the reason on stderr.
func refuse(env *environment, message string) error {
	_, _ = fmt.Fprintln(env.stderr, "githubgate-client:", message)

	_, err := fmt.Fprintln(env.stdout, "quit=1")

	return err
}
