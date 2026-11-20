package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runGH gets a fresh token and runs gh with GH_TOKEN set. With --proxy the
// token is the ServiceAccount token, and gh trusts the proxy CA and the
// system CAs.
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

// proxiedHosts are the GitHub names the proxy answers for. A proxied gh must
// reach the proxy for every one of them.
var proxiedHosts = []string{"api.github.com", "uploads.github.com", "github.com"}

// runProxiedGH runs gh with the ServiceAccount token and a CA bundle of the
// proxy CA plus the system CAs, so gh can follow GitHub's redirects to its
// storage hosts. The system CAs would also accept the real GitHub, so gh runs
// only when every GitHub name resolves to the proxy alone.
func runProxiedGH(ctx context.Context, env *environment, args []string) error {
	caFile, _, err := proxyCA(env)
	if err != nil {
		return err
	}

	token, err := serviceAccountToken(env)
	if err != nil {
		return err
	}

	if err := requireProxyResolution(ctx, env); err != nil {
		return err
	}

	bundleDir, err := os.MkdirTemp("", "githubgate-ca-")
	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(bundleDir) }()

	bundle, err := writeCABundle(env, caFile, bundleDir)
	if err != nil {
		return err
	}

	return execGH(ctx, env, args, "GH_TOKEN="+token, "SSL_CERT_FILE="+bundle)
}

// requireProxyResolution refuses unless every proxied GitHub name resolves
// only to the proxy's addresses.
func requireProxyResolution(ctx context.Context, env *environment) error {
	lookup := env.lookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}

	allowed := map[string]bool{}

	if list := env.getenv("GITHUBGATE_PROXY_ADDRESS"); list != "" {
		for _, address := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
			allowed[normalizeAddress(address)] = true
		}
	} else if host := env.getenv("GITHUBGATE_PROXY_HOST"); host != "" {
		addresses, err := lookup(ctx, host)
		if err != nil {
			return fmt.Errorf("resolve the proxy host %s: %w", host, err)
		}

		for _, address := range addresses {
			allowed[normalizeAddress(address)] = true
		}
	}

	if len(allowed) == 0 {
		return errors.New("gh --proxy needs GITHUBGATE_PROXY_ADDRESS or GITHUBGATE_PROXY_HOST to check where the GitHub names point")
	}

	for _, host := range proxiedHosts {
		addresses, err := lookup(ctx, host)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", host, err)
		}

		if len(addresses) == 0 {
			return fmt.Errorf("%s does not resolve", host)
		}

		for _, address := range addresses {
			if !allowed[normalizeAddress(address)] {
				return fmt.Errorf("%s does not point at the githubgate proxy; add the hostAliases from the proxy mode guide", host)
			}
		}
	}

	return nil
}

func normalizeAddress(address string) string {
	if parsed, err := netip.ParseAddr(strings.TrimSpace(address)); err == nil {
		return parsed.Unmap().String()
	}

	return strings.TrimSpace(address)
}

// systemCABundles are the usual places of the system CA bundle.
var systemCABundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// writeCABundle writes the proxy CA followed by the system CAs to a file in
// dir and returns its path. System CAs come from SSL_CERT_FILE and
// SSL_CERT_DIR when set, else from the usual bundle locations.
func writeCABundle(env *environment, proxyCAFile, dir string) (string, error) {
	proxyPEM, err := os.ReadFile(proxyCAFile)
	if err != nil {
		return "", fmt.Errorf("read the proxy CA file: %w", err)
	}

	files := []string{}
	if file := env.getenv("SSL_CERT_FILE"); file != "" {
		files = append(files, file)
	} else {
		files = append(files, systemCABundles...)
	}

	if certDir := env.getenv("SSL_CERT_DIR"); certDir != "" {
		for _, entry := range filepath.SplitList(certDir) {
			children, _ := os.ReadDir(entry)
			for _, child := range children {
				files = append(files, filepath.Join(entry, child.Name()))
			}
		}
	}

	bundle := append([]byte{}, proxyPEM...)
	bundle = append(bundle, '\n')

	found := false

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil || !bytes.Contains(content, []byte("BEGIN CERTIFICATE")) {
			continue
		}

		found = true

		bundle = append(bundle, content...)
		bundle = append(bundle, '\n')
	}

	if !found {
		return "", errors.New("found no system CA certificates; set SSL_CERT_FILE to a CA bundle")
	}

	path := filepath.Join(dir, "ca-bundle.crt")

	return path, os.WriteFile(path, bundle, 0o600)
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
