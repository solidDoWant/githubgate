# Use proxy mode and signed commits

For: people who run `git` and `gh` in a pod and whose admin turned on proxy mode for them. You know `git`, `gh` and pod specs.

In proxy mode, githubgate sits between your pod and GitHub. Your pod sends its ServiceAccount token where it would send a GitHub token. githubgate checks the request against your policy. If the policy allows it, githubgate sends the request to GitHub with a real token. Your pod never sees a GitHub token.

This page is about proxy mode. For token mode, see [Get GitHub access](getting-access.md). For all commands and settings, see [the client](client.md).

## What you need from your admin

- A `GitHubAccessPolicy` in your namespace with a rule that has `proxy.enabled: true` and names your ServiceAccount.
- The cluster IP of the `githubgate-proxy` Service.
- The proxy CA certificate. It is the certificate authority that signed the githubgate proxy certificate. Ask for a ConfigMap in your namespace that holds it as `ca.crt`.

## Set up a pod

Your pod needs three things:

1. `hostAliases` that point `github.com`, `api.github.com` and `uploads.github.com` to the proxy cluster IP.
2. The proxy CA at `/var/run/secrets/githubgate-proxy/ca.crt`.
3. A projected ServiceAccount token at `/var/run/secrets/githubgate/token`, with the audience that your admin set for githubgate.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: agent
spec:
  serviceAccountName: agent
  hostAliases:
    - ip: 10.96.0.50 # the cluster IP of the githubgate-proxy Service
      hostnames:
        - github.com
        - api.github.com
        - uploads.github.com
  containers:
    - name: work
      image: your-image-with-git-gh-and-githubgate-client
      env:
        - name: GITHUBGATE_PROXY_ADDRESS
          value: 10.96.0.50 # the same cluster IP as in hostAliases
      volumeMounts:
        - name: githubgate-token
          mountPath: /var/run/secrets/githubgate
          readOnly: true
        - name: githubgate-proxy-ca
          mountPath: /var/run/secrets/githubgate-proxy
          readOnly: true
  volumes:
    - name: githubgate-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: githubgate
              expirationSeconds: 3600
    - name: githubgate-proxy-ca
      configMap:
        name: githubgate-proxy-ca
```

The client trusts only the proxy CA file for GitHub, so a request that reaches the real github.com by mistake fails and your token does not leave the pod. To use another path, set `GITHUBGATE_PROXY_CA_FILE`.

`gh --proxy` also trusts the system roots, so that it can follow GitHub's redirects to its download hosts. To keep your token safe, it first checks that the three GitHub names point at the proxy. Set `GITHUBGATE_PROXY_ADDRESS` in the pod to the same cluster IP as in `hostAliases`, or set `GITHUBGATE_PROXY_HOST` to the name of the proxy Service. Without the `hostAliases`, `gh --proxy` stops before it sends the token.

## Use git and gh

Run `configure` once:

```
githubgate-client configure --proxy
```

Then `git clone`, `git fetch` and `git push` work as usual. For `gh`, run `githubgate-client gh --proxy -- pr create --fill`.

If your rule sets `publicRead` or `webPaths`, you can also read public repositories of other owners and download from `github.com`. See [Public repositories and web pages](interception.md#public-repositories-and-web-pages).

## Push signed commits

A plain `git push` works, but GitHub does not sign those commits. To get commits that GitHub shows as Verified, run:

```
githubgate-client push --proxy
```

Run it in a clone with a branch checked out. The client creates your new commits again through the GitHub API. GitHub signs them. The App becomes the author. Your name stays in a `Co-authored-by:` line of the message. The commit ids change, and the client moves your local branch to the new commits. Messages, file contents and file modes stay the same.

`push` refuses a merge commit, a submodule change and a detached `HEAD`. If the remote branch moved while `push` ran, nothing changes. Fetch, rebase and run `push` again.

## GraphQL requests

`gh pr create`, `gh pr view` and many other `gh` commands send GraphQL requests. GraphQL has no repository in its URL, so githubgate can not tell which repository a request is for. It does this instead:

- It uses only the rules that name repositories exactly. A rule with a `*` pattern, for example `toolgate*`, does not count.
- It mints one token that covers all of those repositories. The token gets the lowest permission level that your rules give across them.

So one read-only rule can break writes for the others. Suppose a rule gives `pull_requests: write` on `app`, and another rule gives only `contents: read` on `plugins`. The token covers both repositories with `contents: read` only. `gh pr create` then fails with `Resource not accessible by integration`.

To fix this, do one of these:

- Put the read-only repositories in a rule that uses a `*` pattern, for example `toolgate*`. GraphQL ignores that rule.
- Give all the exact repositories the same permissions.

## Lists with more than one page

GitHub sends the link to the next page of a list with the repository ID instead of its name, for example `https://api.github.com/repositories/1446188295/issues/comments?page=2`. githubgate finds the repository for the ID and applies your rules to it as if you used the name. So `gh api --paginate` and other clients that follow these links work as usual. If no rule covers the repository, githubgate refuses the request with `denied`, and the message gives only the ID. If an admin adds a repository to the installation, githubgate can refuse its ID for up to one minute after the change. A request by ID always goes to the repository that has that ID now. If the repository was renamed and another repository now has its old name, githubgate refuses the request or decides it for the renamed repository, and never sends it to the other repository.

## Read a refusal

When your policy does not allow a request, githubgate answers with a status and a reason. The reason names the repository, the operation, the mutation or the ref. For an API request, the body is JSON with `message` and `error`.

| Code | Status | What it means | What to do |
|---|---|---|---|
| `unauthenticated` | 401 | The ServiceAccount token is missing, expired or for another audience. | Check the token mount and its audience. |
| `denied` | 403 | No rule allows it. The operation or mutation may be denied, or the ref may be outside `pushRefs`. | Ask for less, or ask your admin to change the rule. |
| `unknown_operation` | 403 | githubgate does not know the request, so it does not send it. | Use a different call, or ask your admin. |
| `ambiguous_installation` | 409 | More than one installation fits. | Ask your admin to remove the overlap. |
| `invalid_request` | 400 | The request or a push command list is not valid. | Correct the request. |
| `too_large` | 413 | A GraphQL body is over 1 MiB, or the query that githubgate makes again from it is over 1 MiB. Or a push has more than 1000 refs or more than 64 KiB of commands. | Send less in one request. |
| `unavailable`, `not_ready`, `github_unavailable` | 503 | githubgate or GitHub cannot decide now. | Try again. |

For a refused `git push`, see [A refused push in proxy mode](getting-access.md#a-refused-push-in-proxy-mode).
