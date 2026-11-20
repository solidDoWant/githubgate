package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	helperKey      = "credential.https://github.com.helper"
	useHTTPPathKey = "credential.https://github.com.useHttpPath"
	helperValue    = "!githubgate-client git-credential"
)

// runConfigure sets the git credential helper for github.com in the user's
// global git config. Other keys stay as they are.
func runConfigure(ctx context.Context, env *environment) error {
	// The empty entry resets helpers set at lower levels, then ours follows.
	steps := [][]string{
		{"config", "--global", "--replace-all", helperKey, ""},
		{"config", "--global", "--add", helperKey, helperValue},
		{"config", "--global", "--replace-all", useHTTPPathKey, "true"},
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

	_, err := fmt.Fprintln(env.stdout, "git is set up to get github.com credentials from githubgate")

	return err
}
