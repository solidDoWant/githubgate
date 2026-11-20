package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	helperKey      = "credential.https://github.com.helper"
	useHTTPPathKey = "credential.https://github.com.useHttpPath"
	sslCAInfoKey   = "http.https://github.com/.sslCAInfo"
	helperValue    = "!githubgate-client git-credential"
)

// runConfigure sets the git credential helper for github.com in the user's
// global git config. Other keys stay as they are. With --proxy the helper
// answers the ServiceAccount token, and git trusts only the proxy CA for
// github.com.
func runConfigure(ctx context.Context, env *environment, args []string) error {
	proxy := false

	for _, arg := range args {
		if arg != "--proxy" {
			return errors.New("usage: configure [--proxy]")
		}

		proxy = true
	}

	helper := helperValue
	caFile := ""

	if proxy {
		path, _, err := proxyCA(env)
		if err != nil {
			return err
		}

		if caFile, err = filepath.Abs(path); err != nil {
			return err
		}

		helper += " --proxy"
	}

	// The empty entry resets helpers set at lower levels, then ours follows.
	steps := [][]string{
		{"config", "--global", "--replace-all", helperKey, ""},
		{"config", "--global", "--add", helperKey, helper},
		{"config", "--global", "--replace-all", useHTTPPathKey, "true"},
	}

	if proxy {
		steps = append(steps, []string{"config", "--global", "--replace-all", sslCAInfoKey, caFile})
	}

	for _, step := range steps {
		if out, err := exec.CommandContext(ctx, "git", step...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(step[:3], " "), err, strings.TrimSpace(string(out)))
		}
	}

	if env.getenv("GIT_ASKPASS") != "" {
		_, _ = fmt.Fprintln(env.stderr, "warning: GIT_ASKPASS is set; githubgate-client git-credential stops git from using it for github.com, but other tools may still use it")
	}

	if out, err := exec.CommandContext(ctx, "git", "config", "--local", "--get-all", helperKey).Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		_, _ = fmt.Fprintln(env.stderr, "warning: this repository sets its own credential helper for github.com; it may override githubgate")
	}

	message := "git is set up to get github.com credentials from githubgate"
	if proxy {
		message = "git is set up to reach github.com through the githubgate proxy"
	}

	_, err := fmt.Fprintln(env.stdout, message)

	return err
}
