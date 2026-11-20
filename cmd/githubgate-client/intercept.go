package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/solidDoWant/githubgate/internal/intercept"
)

const (
	defaultInterceptListen    = intercept.DefaultListen
	defaultInterceptTokenFile = "/var/run/secrets/githubgate/token"
	defaultTrustDir           = "/var/run/githubgate/trust"
	defaultStateDir           = "/var/run/githubgate/state"
)

// runIntercept runs the interceptor, or with --probe checks a running one.
func runIntercept(ctx context.Context, env *environment, args []string) error {
	flags := flag.NewFlagSet("intercept", flag.ContinueOnError)
	flags.SetOutput(env.stderr)

	listen := flags.String("listen", defaultInterceptListen, "address to listen on")
	proxyAddress := flags.String("proxy-address", env.getenv("GITHUBGATE_INTERCEPT_PROXY_ADDRESS"), "the githubgate proxy's host:port")
	proxyCA := flags.String("proxy-ca", env.getenv("GITHUBGATE_INTERCEPT_PROXY_CA"), "PEM text or @file: the only roots used to verify the proxy")
	tokenFile := flags.String("token-file", defaultInterceptTokenFile, "the ServiceAccount token file")
	trustDir := flags.String("trust-dir", defaultTrustDir, "directory for the files the workload reads")
	stateDir := flags.String("state-dir", defaultStateDir, "directory for the CA key")
	baseBundle := flags.String("base-bundle", "", "PEM file of roots for bundle.crt (default: built-in roots)")
	extraCAs := []string{}

	if value := env.getenv("GITHUBGATE_INTERCEPT_EXTRA_CA"); value != "" {
		extraCAs = append(extraCAs, value)
	}

	flags.Func("extra-ca", "PEM text or @file appended to bundle.crt; repeatable", func(value string) error {
		extraCAs = append(extraCAs, value)

		return nil
	})

	var joins []intercept.Join

	flags.Func("join", "<container>/<VARIABLE>=<path>: write trust/join/<container>/<VARIABLE> as that file plus the pod CA; repeatable", func(value string) error {
		join, err := intercept.ParseJoin(value)
		if err != nil {
			return err
		}

		joins = append(joins, join)

		return nil
	})
	probe := flags.Bool("probe", false, "check that the interceptor is ready, then exit")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 0 {
		return errors.New("intercept takes no arguments")
	}

	if _, err := intercept.ListenAddress(*listen); err != nil {
		return fmt.Errorf("--listen: %w", err)
	}

	if *probe {
		return intercept.Probe(ctx, *listen, *trustDir)
	}

	if *proxyAddress == "" {
		return errors.New("--proxy-address (or GITHUBGATE_INTERCEPT_PROXY_ADDRESS) is required")
	}

	proxyCAPEM, err := pemValue(*proxyCA)
	if err != nil {
		return fmt.Errorf("--proxy-ca: %w", err)
	}

	if _, err := intercept.ProxyCAPool(proxyCAPEM); err != nil {
		return fmt.Errorf("--proxy-ca: %w", err)
	}

	var extraPEM []byte

	for _, value := range extraCAs {
		pemData, err := pemValue(value)
		if err != nil {
			return fmt.Errorf("--extra-ca: %w", err)
		}

		extraPEM = append(append(extraPEM, pemData...), '\n')
	}

	var base []byte
	if *baseBundle != "" {
		if base, err = os.ReadFile(*baseBundle); err != nil {
			return fmt.Errorf("--base-bundle: %w", err)
		}
	}

	listener, err := env.listener(ctx, *listen)
	if err != nil {
		return err
	}

	return intercept.Run(ctx, intercept.Options{
		Listener:     listener,
		ProxyAddress: *proxyAddress,
		ProxyCA:      proxyCAPEM,
		TokenFile:    *tokenFile,
		TrustDir:     *trustDir,
		StateDir:     *stateDir,
		BaseBundle:   base,
		ExtraCA:      extraPEM,
		Joins:        joins,
		Logger:       slog.New(slog.NewTextHandler(env.stderr, nil)),
	})
}

// pemValue returns PEM given as text, or read from the file named after
// an "@".
func pemValue(value string) ([]byte, error) {
	if path, ok := strings.CutPrefix(value, "@"); ok {
		return os.ReadFile(path)
	}

	return []byte(value), nil
}

// listener opens the interceptor's listener; tests replace it to learn the
// address.
func (e *environment) listener(ctx context.Context, address string) (net.Listener, error) {
	if e.listen != nil {
		return e.listen(ctx, address)
	}

	return intercept.Listen(ctx, address)
}
