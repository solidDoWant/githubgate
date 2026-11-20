package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

type permissionFlags map[string]string

func (p permissionFlags) String() string { return "" }

func (p permissionFlags) Set(value string) error {
	name, level, ok := strings.Cut(value, "=")
	if !ok || name == "" || level == "" {
		return fmt.Errorf("permission %q must look like name=level", value)
	}

	p[name] = level

	return nil
}

type repoFlags []string

func (r *repoFlags) String() string { return "" }

func (r *repoFlags) Set(value string) error {
	*r = append(*r, value)

	return nil
}

func runToken(ctx context.Context, env *environment, args []string) error {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	flags.SetOutput(env.stderr)

	var repos repoFlags

	permissions := permissionFlags{}
	installation := flags.String("installation", "", "installation as namespace/name")
	owner := flags.String("owner", "", "repository owner")

	flags.Var(&repos, "repo", "repository name (repeatable)")
	flags.Var(permissions, "permission", "permission as name=level (repeatable)")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	request := tokenRequest{Installation: *installation, Owner: *owner, Repositories: repos}
	if len(permissions) > 0 {
		request.Permissions = permissions
	}

	response, err := requestToken(ctx, env, request)
	if err != nil {
		return err
	}

	return printLine(env.stdout, response.Token)
}

func printLine(w io.Writer, text string) error {
	_, err := fmt.Fprintln(w, text)

	return err
}
