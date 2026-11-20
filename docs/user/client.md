# githubgate-client

For: people who use `git`, `gh` or a token from a pipeline or a workspace. You know `git` and `gh`.

`githubgate-client` is a small program. It gets a GitHub token from githubgate for the repository you work in. It proves who you are with the ServiceAccount token of your pod. githubgate gives a token only if your namespace has a policy that allows it.

## Settings

Each setting is an environment variable.

- `GITHUBGATE_URL`: the address of githubgate. The default is `https://githubgate.githubgate.svc:8443`. The client refuses an `http://` address, because it would send your ServiceAccount token in clear text.
- `GITHUBGATE_CA_FILE`: the certificate authority file for githubgate. The default is `/var/run/secrets/githubgate/ca.crt`. The client trusts it in addition to the system roots.
- `GITHUBGATE_TOKEN_FILE`: the ServiceAccount token file. The default is `/var/run/secrets/githubgate/token`. The client reads it on every call, so a rotated token works without a restart.

If githubgate cannot be reached, or answers `503 not_ready`, the client tries once more after 200 to 500 ms.

## Commands

- `githubgate-client configure`: sets up `git` for github.com in your global `git` configuration. It keeps your other keys. It replaces other credential helpers for github.com. It warns you if `GIT_ASKPASS` or a repository-local helper is set.
- `githubgate-client token [--repo NAME] [--owner OWNER] [--permission NAME=LEVEL]... [--installation NAMESPACE/NAME]`: prints a token and nothing else.
- `githubgate-client git-credential get`: the `git` credential helper. You do not run it yourself. When githubgate refuses, it prints `quit=1`, so `git` does not ask other helpers or `GIT_ASKPASS`. It prints the reason of githubgate on stderr.
- `githubgate-client gh [--repo OWNER/NAME] -- ARGS`: runs `gh ARGS` with a fresh `GH_TOKEN`. Without `--repo`, it uses the `origin` remote of the current directory if that remote is on github.com.
