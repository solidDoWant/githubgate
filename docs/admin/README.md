# githubgate for admins

For: people who install githubgate and decide who gets GitHub access. You know Kubernetes, Helm and GitHub Apps.

- [Set up token mode](token-mode-setup.md): install, create an App, an installation with a ceiling and a policy, and read the status.

This page also describes the audit log and the metrics.

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

## Server flags

The chart sets these. You need them only to run githubgate yourself.

- `--admin-namespaces`: the namespaces where githubgate reads `GitHubApp`, `GitHubInstallation` and Secrets. Default: the namespace in `POD_NAMESPACE`.
- `--audience`: the audience of the ServiceAccount tokens that callers send. Default: `githubgate`.
- `--tls-cert-file`, `--tls-key-file`: the certificate of the token API. Required. githubgate exits with an error that names the file when it cannot read it.
- `--shutdown-delay`: how long githubgate keeps serving after `SIGTERM` while `/readyz` fails. Default: `5s`. After that it shuts down, within 20 seconds.
- `--leader-elect`: only the elected replica writes status. Needs `POD_NAMESPACE`.
- `--webhook`, `--webhook-cert-dir`, `--webhook-port`: serve the admission webhook at `/validate` (default port 9443).
- `--api-address` (`:8443`), `--health-address` (`:8080`), `--github-base-url` (default: api.github.com).

More pages:

- [Chart values](chart-values.md): every Helm value (generated).

[`SPEC.md`](../../SPEC.md) has the full rules.
