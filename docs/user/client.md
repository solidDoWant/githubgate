# githubgate-client

For: people who use `git`, `gh` or a token from a pipeline or a workspace. You know `git` and `gh`.

`githubgate-client` is a small program. It gets a GitHub token from githubgate for the repository you work in. It proves who you are with the ServiceAccount token of your pod. githubgate gives a token only if your namespace has a policy that allows it.

## Settings

Each setting is an environment variable.

- `GITHUBGATE_URL`: the address of githubgate. The default is `https://githubgate.githubgate.svc:8443`. The client refuses an `http://` address, because it would send your ServiceAccount token in clear text.
- `GITHUBGATE_CA_FILE`: the certificate authority file for githubgate. The default is `/var/run/secrets/githubgate/ca.crt`. The client trusts it in addition to the system roots.
- `GITHUBGATE_PROXY_ADDRESS` or `GITHUBGATE_PROXY_HOST`: where the proxy is. `gh --proxy` needs one of them. `GITHUBGATE_PROXY_ADDRESS` is the proxy's IP address (a comma-separated list is allowed). `GITHUBGATE_PROXY_HOST` is the name of the proxy Service. The client resolves it to find the addresses.
- `GITHUBGATE_TOKEN_FILE`: the ServiceAccount token file. The default is `/var/run/secrets/githubgate/token`. The client reads it on every call, so a rotated token works without a restart.
- `GITHUBGATE_PROXY_CA_FILE`: the certificate authority file for the githubgate proxy. The default is `/var/run/secrets/githubgate-proxy/ca.crt`. The commands with `--proxy` need it. `git` and `push` trust only this file, not the system roots. `gh --proxy` also trusts the system roots, so it can download release assets and archives, and it checks the host names first (see the next item). If a request reaches the real github.com by mistake, it then fails, and your ServiceAccount token does not go to GitHub.

In proxy mode, githubgate forwards your requests to GitHub itself. You send your ServiceAccount token where a GitHub token goes. Your admin tells you if your policy uses proxy mode. Add `--proxy` to the commands below to use it. With `--proxy`, the client never asks githubgate for a token.

If githubgate cannot be reached, or answers `503 not_ready`, the client tries once more after 200 to 500 ms.

## Commands

- `githubgate-client configure [--proxy]`: sets up `git` for github.com in your global `git` configuration. It keeps your other keys. It replaces other credential helpers for github.com. It warns you if `GIT_ASKPASS` or a repository-local helper is set. With `--proxy`, it sets the helper `git-credential --proxy` and makes `git` trust only the proxy CA file for github.com. It fails if the proxy CA file has no certificate. `githubgate-client` must be on your `PATH`.
- `githubgate-client token [--repo NAME] [--owner OWNER] [--permission NAME=LEVEL]... [--installation NAMESPACE/NAME]`: prints a token and nothing else.
- `githubgate-client git-credential [--proxy] get`: the `git` credential helper. You do not run it yourself. When githubgate refuses, it prints `quit=1`, so `git` does not ask other helpers or `GIT_ASKPASS`. It prints the reason of githubgate on stderr. With `--proxy`, it gives your ServiceAccount token as the password, only for `https://github.com`. For any other host, it gives nothing.
- `githubgate-client gh [--repo OWNER/NAME | --proxy] -- ARGS`: runs `gh ARGS` with a fresh `GH_TOKEN`. Without `--repo`, it uses the `origin` remote of the current directory if that remote is on github.com. With `--proxy` instead of `--repo`, it runs `gh` with your ServiceAccount token as `GH_TOKEN`, and `gh` trusts the proxy CA file and the system roots. Before it runs `gh`, the client resolves `api.github.com`, `uploads.github.com` and `github.com`. If any address is not the proxy's, it names the host and stops, so your token never goes to the real GitHub.
- `githubgate-client push [--proxy] [REMOTE [BRANCH]]`: publishes your local commits so that GitHub signs them. Run it in a clone whose remote is on github.com. The default remote is `origin`. `BRANCH` must be the branch that you checked out. The client takes a token with `contents: write` for the repository. Then it creates each commit that the remote branch does not have through the GitHub API, with the same message, files, file modes and deletions. GitHub marks these commits as Verified. The App is the author, and your name stays in a `Co-authored-by:` line of the message. The client moves the remote branch once, without force. If the remote branch moved meanwhile, nothing changes. Afterward the client moves your local branch to the new commits if the files match. It does not touch your index or working tree. `push` refuses a detached `HEAD`, a merge commit, a submodule change and a file over 100 MB. It names the commit and publishes nothing. If GitHub answers with a rate limit, the client prints the message and publishes nothing. Set `GITHUBGATE_GITHUB_API_URL` to use another GitHub API root. The client accepts an `http://` address only for `localhost`. With `--proxy`, the requests go through the proxy with your ServiceAccount token, and the address must use `https://`. The proxy then checks that your policy's `pushRefs` allow the branch. Run `githubgate-client configure --proxy` first, because `push` also runs `git fetch`.

## The interceptor

`githubgate-client intercept` runs the interceptor. The interceptor is a small server next to your workload. It answers HTTPS for `api.github.com`, `uploads.github.com` and `github.com`. It removes every credential that your tools send (the `Authorization`, `Proxy-Authorization` and `Cookie` headers). Then it sends the request to the githubgate proxy with your ServiceAccount token. You do not usually run it yourself. In an opted-in namespace, githubgate adds it to your pod.

At start, the interceptor does these steps:

1. It reads the certificate authority (CA) of the proxy. If the CA has no certificate, the interceptor stops with an error. The interceptor trusts only this CA for the proxy, not the system roots.
2. It loads its own CA from the state directory. If the CA is missing, does not parse, or has seven days or less left, the interceptor makes a new CA. The new CA is valid for 10 years. Its key stays in the state directory.
3. It makes a server certificate for the three GitHub hosts. The certificate is valid for 30 days and stays in memory. The interceptor makes a new one seven days before it expires.
4. It writes `ca.crt` (its own CA), `bundle.crt` (the base roots, the extra CAs and its own CA) and then `ready` to the trust directory.

The interceptor reads the token file on every request, so a rotated token works without a restart.

Flags:

- `--listen`: the address to listen on. The default is `127.0.0.1:443`.
- `--proxy-address`, or the variable `GITHUBGATE_INTERCEPT_PROXY_ADDRESS`: the `host:port` of the proxy Service.
- `--proxy-ca`, or the variable `GITHUBGATE_INTERCEPT_PROXY_CA`: the CA of the proxy, as PEM text or as `@FILE`.
- `--token-file`: the ServiceAccount token file. The default is `/var/run/secrets/githubgate/token`.
- `--trust-dir`: the directory for the files that your tools read. The default is `/var/run/githubgate/trust`.
- `--state-dir`: the directory for the CA key. The default is `/var/run/githubgate/state`. Only the interceptor must be able to read it.
- `--base-bundle`: a PEM file of roots for `bundle.crt`. The default is the built-in roots of the `golang.org/x/crypto` fallback bundle. Roots that have an extra limit in that bundle, such as a distrust date, are left out.
- `--extra-ca`, or the variable `GITHUBGATE_INTERCEPT_EXTRA_CA`: more CA certificates for `bundle.crt`, as PEM text or as `@FILE`. Use it for private CAs.
- `--probe`: makes sure that the interceptor is ready, then exits. It exits 0 only if the `ready` file exists and the interceptor on `--listen` shows a certificate for `api.github.com` that `ca.crt` signed.

The interceptor answers these errors itself. On the API hosts, the answer is JSON. On `github.com`, the answer is plain text.

| Condition | Answer |
|---|---|
| The token file is missing, empty or cannot be read. Nothing goes to the proxy. | `503`, "githubgate: can't read the ServiceAccount token" |
| The proxy cannot be reached, its certificate does not verify, or it does not answer in time. | `503`, "githubgate: proxy unavailable" |
| The `Host` header is not one of the three GitHub hosts. | `421` |
