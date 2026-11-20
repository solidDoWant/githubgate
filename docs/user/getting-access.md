# Get GitHub access in a task or a workspace

For: people who run `git` and `gh` in a Tekton task or a Coder workspace. You know `git`, `gh` and your pipeline.

githubgate gives your pod a GitHub token that is limited to the repositories and permissions that your admin allowed. You never hold a GitHub App key. Your pod proves who it is with the token of its ServiceAccount.

## What you need from your admin

Ask your admin for these items:

- A `GitHubAccessPolicy` in your namespace that names the ServiceAccount your pod runs as. The policy lists the repositories and permissions you get.
- The address of githubgate and its certificate authority (CA). The certificate authority is the certificate that signs the githubgate server certificate. The chart publishes it in the ConfigMap `githubgate-ca`.

Your pod must mount the ServiceAccount token at `/var/run/secrets/githubgate/token` and the CA at `/var/run/secrets/githubgate/ca.crt`. The token must be a projected token with the audience that your admin configured for githubgate. If the address is not `https://githubgate.githubgate.svc:8443`, set `GITHUBGATE_URL`. See [the client](client.md) for all settings.

## In a Tekton task

Use a step image that has `git`, `gh` and `githubgate-client`. Run the task with the ServiceAccount that your policy names. A step can look like this:

```yaml
steps:
  - name: clone
    image: your-image-with-git-and-githubgate-client
    script: |
      githubgate-client configure
      git clone https://github.com/OWNER/foo
```

1. Run `githubgate-client configure` once in the step. It sets `git` to ask githubgate for a token for each repository.
2. Run `git clone`, `git fetch` and `git push` as usual.
3. To use `gh`, run `githubgate-client gh -- pr create --fill`. The client gets a fresh token for each call, so it also works in a step that runs longer than one hour.
4. To get a token for a tool that is not `git` or `gh`, run `githubgate-client token --repo foo --permission contents=read`. It prints the token and nothing else.

A token lives at most one hour. Do not store it in a result or a workspace that other tasks read.

## In a Coder workspace

A workspace that runs as a pod works like a task. Give the workspace pod a ServiceAccount that a policy names, mount the token and the CA at the paths above, and run `githubgate-client configure` in the startup script. Then use `git` and `githubgate-client gh -- ...` in the terminal.

`githubgate-client configure` changes your global `git` configuration. It replaces other credential helpers for github.com. It warns you if `GIT_ASKPASS` or a repository-local helper is set, because those can take priority.

## Read a refusal

When githubgate refuses, `git` fails with its normal authentication error. The client prints the reason of githubgate on stderr. The reason has a code and a message that names your namespace, your ServiceAccount, and the repository or permission that was not allowed.

| Code | Status | What it means | What to do |
|---|---|---|---|
| `unauthenticated` | 401 | The ServiceAccount token is missing, expired, for another audience, or for a deleted ServiceAccount. | Make sure that the token is mounted with the right audience. |
| `denied` | 403 | No accepted rule of your policies covers the request. The message names the repository, or the permission and level. | Ask for less, or ask your admin to change your policy. |
| `ambiguous_installation` | 409 | The owner has more than one installation that you can use. | Run the command again with `--installation NAMESPACE/NAME`. |
| `invalid_request` | 400 | The request is not valid, for example a bad repository name. | Correct the request. |
| `github_refused` | 403 | GitHub refused to make the token. | Tell your admin. |
| `not_ready`, `unavailable`, `github_unavailable` | 503 | githubgate or GitHub cannot decide now. | Try again. The client tries once by itself. |

githubgate refuses when it cannot decide. It never gives a wider token to make a request work.

If `denied` names a repository that your policy lists, ask your admin to check the status of the policy. A rule that is outside the ceiling of the installation is not accepted and gives nothing. See [the admin guide](../admin/token-mode-setup.md).

A policy change applies to new tokens only. A token that githubgate already gave out stays valid until it expires.
