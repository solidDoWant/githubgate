# githubgate for admins

For: people who install githubgate and decide who gets GitHub access. You know Kubernetes, Helm and GitHub Apps.

githubgate is in early development. These pages are not written yet. They will show:

- how to install githubgate with its Helm chart;
- how to create a GitHub App, its `GitHubApp` and `GitHubInstallation` objects, and a ceiling;
- how to write a `GitHubAccessPolicy` for a namespace;
- how to read the status of the objects, the audit log and the metrics.

## The audit log

githubgate writes one JSON line to its log for each token request. The line never holds a token, a key or a header value. It has these fields:

- `namespace`, `service_account` and `pod`: the caller.
- `installation`: the `GitHubInstallation` as `namespace/name`.
- `mode`: `token`.
- `repositories` and `permissions`: what the caller got, or what it asked for when githubgate refused.
- `rules`: the policy rules that gave the access, as `namespace/policy/rule`.
- `decision` (`allowed` or `refused`), `reason` (the error code, or `allowed`) and `status` (the HTTP status).
- `github_status`: `minted` when GitHub made a new token, `cached` when githubgate gave out a token it already had, `not_called` when GitHub was not asked, or the GitHub error code.
- `duration` and `request_id`.

## The metrics

githubgate serves Prometheus metrics at `/metrics` on its health port (8080):

- `githubgate_requests_total{mode,decision,code_class}`: the requests, by decision and by HTTP status class.
- `githubgate_github_request_duration_seconds{operation}`: the time that GitHub takes to make a token.
- `githubgate_token_cache_total{result}`: the token cache lookups, with `result` `hit` or `miss`.
- `githubgate_tokenreview_total{result}`: the caller authentications, by result.
- `githubgate_github_rate_limit_remaining{installation}`: the requests that are left in the GitHub rate limit of an installation.

`/readyz` fails until githubgate has read its configuration, and at once when the pod gets `SIGTERM`.

More pages:

- [Chart values](chart-values.md): every Helm value (generated).

[`SPEC.md`](../../SPEC.md) has the full rules.
