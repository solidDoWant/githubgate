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
