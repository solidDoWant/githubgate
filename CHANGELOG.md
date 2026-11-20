# Changelog

The release notes of githubgate. Each release has a `## <version>` section, written by the release manager when the release is cut.

## 0.1.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount, and githubgate gives it a short-lived GitHub token. That token covers only the repositories and permissions that the policy of its namespace allows. This is the first release. It covers token mode, which is phase 1 of the project. Install it with the Helm chart:

```
helm install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.1.0 --namespace githubgate --create-namespace
```

The chart needs cert-manager. The setup is in [Set up token mode](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/token-mode-setup.md).

### Tokens

- A workload asks for a token with `POST /v1/token` and its ServiceAccount token. githubgate checks the ServiceAccount token with the Kubernetes `TokenReview` API. ([#39](https://github.com/solidDoWant/githubgate/pull/39), [#51](https://github.com/solidDoWant/githubgate/pull/51))
- githubgate gives a GitHub installation token that covers only the repositories and permissions that the request names and the policy allows. GitHub itself enforces this limit on the token. ([#47](https://github.com/solidDoWant/githubgate/pull/47), [#48](https://github.com/solidDoWant/githubgate/pull/48))
- githubgate refuses a request outside the policy and gives no token. The refusal names the repository or the permission that the policy does not allow. ([#47](https://github.com/solidDoWant/githubgate/pull/47), [#51](https://github.com/solidDoWant/githubgate/pull/51))
- If a request names no repositories, the token covers the repositories that the policy names exactly. ([#47](https://github.com/solidDoWant/githubgate/pull/47))
- Each replica keeps a token and gives it again to the same caller for the same request until less than ten minutes of the token are left. ([#51](https://github.com/solidDoWant/githubgate/pull/51))

### Apps, installations and access policies

- Three custom resources configure githubgate. `GitHubApp` holds an App and its private key. `GitHubInstallation` names the owner where the App is installed and sets the ceiling, which is the most access any policy can get through it. `GitHubAccessPolicy` says which ServiceAccounts of a namespace get which repositories and permissions. ([#37](https://github.com/solidDoWant/githubgate/pull/37), [#44](https://github.com/solidDoWant/githubgate/pull/44))
- githubgate reads `GitHubApp` and `GitHubInstallation` objects, and their Secrets, only in the admin namespaces. ([#44](https://github.com/solidDoWant/githubgate/pull/44))
- A policy rule outside the ceiling of its installation gives nothing. The other rules of the policy still apply. ([#44](https://github.com/solidDoWant/githubgate/pull/44))
- Each object shows its state in a `Ready` condition with a reason. A policy also shows each of its rules as accepted or not. ([#49](https://github.com/solidDoWant/githubgate/pull/49))
- A policy change applies to new tokens within seconds. If the key Secret of an App is broken and you fix it, the App works again without a restart. ([#44](https://github.com/solidDoWant/githubgate/pull/44), [#49](https://github.com/solidDoWant/githubgate/pull/49))
- An optional admission webhook rejects objects that are not valid when you create them. Turn it on with the chart value `webhook.enabled`. ([#45](https://github.com/solidDoWant/githubgate/pull/45))
- A policy can name at most 256 ServiceAccounts and 256 rules. A rule can name at most 256 repositories. ([#68](https://github.com/solidDoWant/githubgate/pull/68))

### Client (`git`, `gh`)

- `githubgate-client` is a small static program for pipelines and workspaces. It is published as the image `ghcr.io/soliddowant/githubgate-client`. ([#41](https://github.com/solidDoWant/githubgate/pull/41))
- `githubgate-client configure` sets up `git` to get its credentials for github.com from githubgate. After that, `git clone`, `git fetch` and `git push` work as usual. ([#41](https://github.com/solidDoWant/githubgate/pull/41))
- `githubgate-client gh -- ARGS` runs `gh` with a fresh token on each call. `githubgate-client token` prints a token and nothing else. ([#41](https://github.com/solidDoWant/githubgate/pull/41))
- When githubgate refuses, `git` stops and the client prints the reason. ([#41](https://github.com/solidDoWant/githubgate/pull/41))

### Deployment

- The Helm chart runs three replicas by default. Every replica serves requests. It installs the CRDs and the RBAC rules, and creates the certificates with cert-manager. ([#40](https://github.com/solidDoWant/githubgate/pull/40), [#55](https://github.com/solidDoWant/githubgate/pull/55))
- The chart value `githubBaseURL` points githubgate at another GitHub API, for example GitHub Enterprise Server. ([#60](https://github.com/solidDoWant/githubgate/pull/60))
- githubgate writes one audit line for each request, and serves Prometheus metrics for requests, the token cache, authentications and the GitHub rate limit. The metrics are in the [admin overview](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/README.md). ([#48](https://github.com/solidDoWant/githubgate/pull/48), [#51](https://github.com/solidDoWant/githubgate/pull/51))
- The guides for workload users and for admins are under `docs/user/` and `docs/admin/`. ([#53](https://github.com/solidDoWant/githubgate/pull/53))

### Security

- The private key of an App never leaves githubgate. githubgate never writes a key or a token to logs, status, events, metrics or errors. ([#48](https://github.com/solidDoWant/githubgate/pull/48), [#51](https://github.com/solidDoWant/githubgate/pull/51))
- One very large policy can no longer make every replica run out of memory. ([#68](https://github.com/solidDoWant/githubgate/pull/68))
- githubgate closes idle connections and keeps at most 1024 connections open on each listener. Callers that do not authenticate can no longer use up its memory. ([#71](https://github.com/solidDoWant/githubgate/pull/71))
- When githubgate cannot list the installations of an App, it tries again at most once every 30 seconds for that App. This keeps it inside the GitHub rate limit. ([#72](https://github.com/solidDoWant/githubgate/pull/72))
- `golang.org/x/net` is now v0.60.0, which fixes known vulnerabilities. ([#67](https://github.com/solidDoWant/githubgate/pull/67))

Internal work: a fake GitHub for tests, end-to-end tests on kind, a live test against a real GitHub App, CI, dependency updates and design documents.

### Known issues

None.
