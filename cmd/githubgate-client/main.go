// Command githubgate-client is the githubgate client: the git credential
// helper, the token command, the gh wrapper and the git setup command.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `usage: githubgate-client <command>

commands:
  token [--repo name] [--owner owner] [--permission name=level]... [--installation ns/name]
  git-credential [--proxy] get|store|erase
  gh [--repo owner/name | --proxy] -- <gh args>
  push [--proxy | --intercepted] [<remote> [<branch>]]
  configure [--proxy]
  intercept [--listen addr] [--proxy-address host:port] [--proxy-ca pem|@file] [--token-file path]
            [--trust-dir dir] [--state-dir dir] [--base-bundle file] [--extra-ca pem|@file]... [--join container/VAR=path]... [--probe]
  version
`

func main() {
	ctx, stop := shutdownContext(context.Background())
	defer stop()

	env := &environment{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		sleep:  sleepContext,
	}

	if err := run(ctx, env, os.Args[1:]); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}

		_, _ = fmt.Fprintln(os.Stderr, "githubgate-client:", err)

		os.Exit(1)
	}
}

// shutdownContext is cancelled on SIGINT and on SIGTERM, which kubelet sends
// to stop a sidecar, so long-running commands shut down gracefully.
func shutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// exitError carries the exit code of a wrapped command.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// environment is everything the client takes from the process, so tests can
// replace it.
type environment struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	sleep  func(context.Context, int) error
	// lookupHost resolves a host name; nil means the system resolver.
	lookupHost func(context.Context, string) ([]string, error)
	// listen opens the interceptor's listener; nil means net.Listen.
	listen func(context.Context, string) (net.Listener, error)
}

func run(ctx context.Context, env *environment, args []string) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.stderr, usage)

		return errors.New("a command is required")
	}

	switch args[0] {
	case "version":
		_, err := fmt.Fprintf(env.stdout, "githubgate-client %s\n", version)

		return err
	case "token":
		return runToken(ctx, env, args[1:])
	case "git-credential":
		return runGitCredential(ctx, env, args[1:])
	case "gh":
		return runGH(ctx, env, args[1:])
	case "push":
		return runPush(ctx, env, args[1:])
	case "configure":
		return runConfigure(ctx, env, args[1:])
	case "intercept":
		return runIntercept(ctx, env, args[1:])
	default:
		_, _ = fmt.Fprint(env.stderr, usage)

		return fmt.Errorf("unknown command %q", args[0])
	}
}
