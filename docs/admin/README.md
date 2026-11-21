# githubgate for admins

For: people who install githubgate and decide who gets GitHub access. You know Kubernetes, Helm and GitHub Apps.

- [Set up token mode](token-mode-setup.md): install, create an App, an installation with a ceiling and a policy, and read the status.
- [Set up proxy mode](proxy-mode-setup.md): the name-constrained CA, the chart values and the proxy rules.
- [Set up user access tokens](user-access-setup.md): the App account permission, device flow, the authorization, the conditions and re-authorizing.
- [Set up interception](interception-setup.md): the injection webhook, opt-in for single pods, and KubeVirt VMs.

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

githubgate also writes one JSON line for each request to the proxy, when the response ends. The line never holds a token, a key, a header value, the path or the query. It has the fields above, with `mode` set to `proxy`, and these fields:

- `host` and `method`: the GitHub host and the HTTP method of the request.
- `kind`: `rest`, `graphql`, `git-info-refs`, `git-upload-pack`, `git-receive-pack` or `web` (a `github.com` web path).
- `access`: how githubgate sent an allowed request. `rules` is a token of the rules. `public_read` is the token for public data, and then `repositories` names the repository that the caller read. `web` is no credential. `user` is the user access token of a `GitHubUserAuthorization`. The field is empty when githubgate refused the request.
- `operation`: the REST operation id, for example `repos/create-release`.
- `user_authorization` and `login`: for a request that a user rule allowed, the `GitHubUserAuthorization` (`namespace/name`) and the GitHub user in its `spec.login`. Both are empty for other requests. The line never holds the user access token.
- `mutations`: the names of the GraphQL mutations in the request.
- `refs`: the git refs that the request writes, when githubgate read them.
- `github_status`: the HTTP status from GitHub, `not_called` when githubgate did not send the request, or the GitHub error code when githubgate could not get a token.
- `bytes`: the size of the response body that the caller got.

## The metrics

githubgate serves Prometheus metrics at `/metrics` on its health port (8080):

- `githubgate_requests_total{mode,decision,code_class}`: the requests, by decision and by HTTP status class.
- `githubgate_github_request_duration_seconds{operation}`: the time that GitHub takes to make a token.
- `githubgate_token_cache_total{result}`: the token cache lookups, with `result` `hit` or `miss`.
- `githubgate_tokenreview_total{result}`: the caller authentications, by result.
- `githubgate_github_rate_limit_remaining{installation}`: the requests that are left in the GitHub rate limit of an installation.
- `githubgate_user_authorization_ready{namespace,name}`: 1 when a `GitHubUserAuthorization` is Ready, and 0 when it is not.
- `githubgate_user_token_refreshes_total{namespace,name,result}`: the refreshes of the user tokens. `result` is `ok`, `failed` (GitHub did not answer, so githubgate tries again later), `revoked` (GitHub refused the refresh token) or `conflict` (a newer token pair was already stored, so githubgate did not store this one).
- `githubgate_user_refresh_token_expiry_seconds{namespace,name}`: the time when the stored refresh token expires, in seconds since the Unix epoch.

Only the leader replica reports the three user authorization metrics. The leader is the one replica that writes status.

`/readyz` fails until githubgate has read its configuration, and at once when the pod gets `SIGTERM`.

## Server flags

The chart sets these. You need them only to run githubgate yourself.

- `--admin-namespaces`: the namespaces where githubgate reads `GitHubApp`, `GitHubInstallation` and Secrets. Default: the namespace in `POD_NAMESPACE`.
- `--audience`: the audience of the ServiceAccount tokens that callers send. Default: `githubgate`.
- `--tls-cert-file`, `--tls-key-file`: the certificate of the token API. Required. githubgate exits with an error that names the file when it cannot read it.
- `--shutdown-delay`: how long githubgate keeps serving after `SIGTERM` while `/readyz` fails. Default: `5s`. After that it shuts down, within 20 seconds.
- Fixed limits, not configurable: each listener (token API and health) keeps at most 1024 connections open and closes the rest at once. It closes a connection that waits 60 seconds for its next request, and the TLS handshake and a request must each finish within 10 seconds.
- `--leader-elect`: only the elected replica writes status. Needs `POD_NAMESPACE`.
- `--webhook`, `--webhook-cert-dir`, `--webhook-port`: serve the admission webhook at `/validate` (default port 9443).
- `--api-address` (`:8443`), `--health-address` (`:8080`), `--github-base-url` (default: api.github.com).
- `--github-oauth-url`: where githubgate runs the device flow for a `GitHubUserAuthorization` (`/login/device/code` and `/login/oauth/access_token`). Default: `https://github.com`.
- `--proxy`: serve proxy mode on `--proxy-address` (default `:9443`). githubgate decides each request against the proxy rules of the caller. It sends an allowed request to GitHub with an installation token in place of the ServiceAccount token.
- `--proxy-upstream-api`, `--proxy-upstream-uploads`, `--proxy-upstream-git`: where the proxy sends requests for `api.github.com`, `uploads.github.com` and `github.com`. Each must be an `https` URL. Defaults: the GitHub hosts. The chart values are `proxy.upstreams.api`, `proxy.upstreams.uploads` and `proxy.upstreams.git`.
- `--proxy-upstream-ca-file`: extra CA certificates that the upstreams can use, added to the system CAs. The chart value `proxy.upstreams.caConfigMap` names a ConfigMap with the certificates in `ca.crt`.
- `--proxy-tls-cert-file`, `--proxy-tls-key-file`: the certificate of the proxy. They are required with `--proxy`. The certificate must name `api.github.com`, `uploads.github.com` and `github.com`. The proxy refuses a TLS connection for any other name. githubgate reads the files again when they change.
- The webhook and the proxy both use port 9443 by default. If you turn on both, set `--webhook-port` or `--proxy-address` to a different port.

More pages:

- [Chart values](chart-values.md): every Helm value (generated).

[`SPEC.md`](../../SPEC.md) has the full rules.
