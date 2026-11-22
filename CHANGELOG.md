# Changelog

The release notes of githubgate. Each release has a `## <version>` section, written by the release manager when the release is cut.

## 0.7.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release fixes VM workspaces, which did not start when githubgate intercepted them. It also makes the injection webhook stricter about the mounts of its own volumes. It has the same product code as the candidate 0.7.1-rc.1 (a test version before the final release). Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.7.1 --namespace githubgate --create-namespace
```

Apps, installations, policies, user authorizations, chart values and clients of 0.7.0 work without a change.

### Deployment

- KubeVirt VM workspaces work again with interception. KubeVirt adds a mount of the trust volume `githubgate-trust` to share the trust directory with the guest. Before, the injection webhook replaced that mount when it ran a second time, so the VM did not start. Now the webhook keeps a mount of the trust volume that another webhook adds at a different path. The setup is in [VM workspaces](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/interception-setup.md). ([#310](https://github.com/solidDoWant/githubgate/pull/310))

### Security

- The injection webhook now removes mounts of `githubgate-token`, `githubgate-intercept-state` and `githubgate-extra-ca` from workload containers each time it runs. It also makes each mount of `githubgate-trust` in a workload container read-only. Before, a pod that set the annotation `githubgate.soliddowant.dev/injected` itself kept these mounts. ([#316](https://github.com/solidDoWant/githubgate/pull/316))
- The interceptor no longer copies a CA file from a volume of githubgate or from a volume that it cannot read. Before, a pod that set the annotation `githubgate.soliddowant.dev/injected` itself could make the interceptor copy a githubgate file into the trust directory, where every container can read it. ([#318](https://github.com/solidDoWant/githubgate/pull/318))

### Fixes

- If a CA variable, for example `SSL_CERT_FILE`, points to a file in a mount of the trust volume that githubgate did not add, the webhook now rejects the pod. Before, the interceptor read the file that it wrote itself, and the file grew on each sync. ([#315](https://github.com/solidDoWant/githubgate/pull/315))
- If a container has a mount at `/var/run/githubgate/trust` that githubgate did not add, the webhook now rejects the pod. The reason starts with `githubgate: pod can't be intercepted` and names the container. Before, the pod had two mounts at the same path, and the API server rejected it with a general validation error. ([#317](https://github.com/solidDoWant/githubgate/pull/317))

Internal work: a CI test no longer prints log lines for connections that it drops on purpose, and CI changes for CPU pinning and for pull request runs ([#309](https://github.com/solidDoWant/githubgate/pull/309), [#322](https://github.com/solidDoWant/githubgate/pull/322), [#323](https://github.com/solidDoWant/githubgate/pull/323)).

### Known issues

- In proxy mode, githubgate can refuse a GraphQL request with the wrong reason. This happens when an exact repository rule refuses the request, for example because of `pushRefs`, and only a pattern rule is left. The caller then gets `403 your proxy rules only use patterns; GraphQL needs exact repository names`. The refusal is correct, but the message does not name the real reason. ([#325](https://github.com/solidDoWant/githubgate/issues/325))

## 0.7.1-rc.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release is a candidate (a test version before the final release) for 0.7.1. It fixes VM workspaces, which did not start when githubgate intercepted them. It also makes the injection webhook stricter about the mounts of its own volumes. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.7.1-rc.1 --namespace githubgate --create-namespace
```

Apps, installations, policies, user authorizations, chart values and clients of 0.7.0 work without a change.

### Deployment

- KubeVirt VM workspaces work again with interception. KubeVirt adds a mount of the trust volume `githubgate-trust` to share the trust directory with the guest. Before, the injection webhook replaced that mount when it ran a second time, so the VM did not start. Now the webhook keeps a mount of the trust volume that another webhook adds at a different path. The setup is in [VM workspaces](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/interception-setup.md). ([#310](https://github.com/solidDoWant/githubgate/pull/310))

### Security

- The injection webhook now removes mounts of `githubgate-token`, `githubgate-intercept-state` and `githubgate-extra-ca` from workload containers each time it runs. It also makes each mount of `githubgate-trust` in a workload container read-only. Before, a pod that set the annotation `githubgate.soliddowant.dev/injected` itself kept these mounts. ([#316](https://github.com/solidDoWant/githubgate/pull/316))
- The interceptor no longer copies a CA file from a volume of githubgate or from a volume that it cannot read. Before, a pod that set the annotation `githubgate.soliddowant.dev/injected` itself could make the interceptor copy a githubgate file into the trust directory, where every container can read it. ([#318](https://github.com/solidDoWant/githubgate/pull/318))

### Fixes

- If a CA variable, for example `SSL_CERT_FILE`, points to a file in a mount of the trust volume that githubgate did not add, the webhook now rejects the pod. Before, the interceptor read the file that it wrote itself, and the file grew on each sync. ([#315](https://github.com/solidDoWant/githubgate/pull/315))
- If a container has a mount at `/var/run/githubgate/trust` that githubgate did not add, the webhook now rejects the pod. The reason starts with `githubgate: pod can't be intercepted` and names the container. Before, the pod had two mounts at the same path, and the API server rejected it with a general validation error. ([#317](https://github.com/solidDoWant/githubgate/pull/317))

Internal work: a CI test no longer prints log lines for connections that it drops on purpose ([#309](https://github.com/solidDoWant/githubgate/pull/309)).

### Known issues

None.

## 0.7.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release adds user access tokens. A user access token is a token of a GitHub App that acts as one GitHub user. With it, a workload in proxy mode can use resources that belong to a user account, for example SSH signing keys. The workload never sees the token. It has the same product code as the candidate 0.7.0-rc.1 (a test version before the final release). Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.7.0 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- If you install the CRDs without the chart, apply the new `crds.yaml` of this release. It now has four CRDs. The new CRD is `GitHubUserAuthorization`. The chart installs it for you. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- If you manage the RBAC of githubgate yourself, let githubgate create, update and patch Secrets in the admin namespaces. Also let it read and watch `githubuserauthorizations` and write their status. githubgate keeps the tokens of a user authorization in a Secret that it creates. The chart sets these permissions for you. Secrets in other namespaces stay closed to githubgate. ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#285](https://github.com/solidDoWant/githubgate/pull/285))
- If you want to use user access tokens, change the settings of your GitHub App first. Add the account permission that you need, for example `SSH signing keys: Read and write`. Turn on `Enable Device Flow`. Keep `Expire user authorization tokens` on. Then give githubgate the client ID of the App in `clientID` or `clientIDSecretRef` of the `GitHubApp`. The steps are in [Set up user access tokens](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/user-access-setup.md). ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#266](https://github.com/solidDoWant/githubgate/pull/266), [#286](https://github.com/solidDoWant/githubgate/pull/286))

Apps, installations, policies, chart values and clients of 0.6.4 work without a change. You do not need user access tokens to upgrade.

### User access tokens

- The new kind `GitHubUserAuthorization` lives in an admin namespace. It names an App, the GitHub user (`login`) who must authorize the App, a ceiling of REST operations, and the namespaces that can use it. The ceiling is the most that any policy can call as this user. It accepts only operations on `/user` paths. ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#295](https://github.com/solidDoWant/githubgate/pull/295))
- A `GitHubApp` has two new fields, `clientID` and `clientIDSecretRef`, for the client ID of the App. Set at most one. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- You authorize the App as the user with GitHub's device flow. You start the flow when you set the annotation `githubgate.soliddowant.dev/authorize` to a new value. The status shows the code and the address where the user enters it. githubgate stores the tokens in the Secret `NAME-user-token`, where `NAME` is the name of the authorization. If a different user authorizes the App, githubgate discards the token and reports `WrongUser`. ([#266](https://github.com/solidDoWant/githubgate/pull/266))
- The leader replica refreshes the user access token before it expires. A refresh token works only once, so githubgate writes each new pair to the Secret at once. If GitHub refuses the refresh token, the authorization reports `RefreshTokenRevoked` and you authorize again. If GitHub refuses a user access token with `401`, the leader refreshes it at once. ([#271](https://github.com/solidDoWant/githubgate/pull/271), [#283](https://github.com/solidDoWant/githubgate/pull/283), [#298](https://github.com/solidDoWant/githubgate/pull/298))
- The conditions of a `GitHubUserAuthorization` tell you what to do, for example `AuthorizationRequired`, `AuthorizationExpired`, `DeviceFlowDisabled` or `SpecChanged`. If you change the App, the client ID or the login after you authorized, githubgate clears the stored tokens and you authorize again. If a new authorization fails while a token is stored, the conditions show the failure. ([#266](https://github.com/solidDoWant/githubgate/pull/266), [#293](https://github.com/solidDoWant/githubgate/pull/293), [#297](https://github.com/solidDoWant/githubgate/pull/297))
- New metrics: `githubgate_user_authorization_ready`, `githubgate_user_token_refreshes_total` and `githubgate_user_refresh_token_expiry_seconds`. Only the leader replica reports them. ([#271](https://github.com/solidDoWant/githubgate/pull/271))

### Access policies

- A policy rule can now have `userAuthorizationRef` in place of `installationRef`. This is a user rule. It works in proxy mode only, and it lists exact operations in `proxy.allowOperations`. The authorization must list each operation in its ceiling and must admit the namespace of the policy. Otherwise the rule is not accepted, and the status of the policy says why. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- In proxy mode, githubgate sends a REST request that a user rule allows with the user access token. A request that no user rule lists never gets a user token. If the token is missing, expired or revoked, the caller gets `503`, and githubgate never sends the request with an installation token. The audit line records `access: "user"`, the authorization and the login, but never the token. The steps for workload users are in [Use a user rule](https://github.com/solidDoWant/githubgate/blob/master/docs/user/user-rules.md). ([#281](https://github.com/solidDoWant/githubgate/pull/281), [#286](https://github.com/solidDoWant/githubgate/pull/286))

### Deployment

- New chart value `githubOAuthURL` and server flag `--github-oauth-url`. They set the host of the device flow. The default is `https://github.com`. Set them only for GitHub Enterprise Server or a test double. ([#266](https://github.com/solidDoWant/githubgate/pull/266), [#285](https://github.com/solidDoWant/githubgate/pull/285))

### Security

- githubgate never follows a redirect on a call to the OAuth endpoints of GitHub. A redirect cannot send a refresh token or a device code to a different host. ([#292](https://github.com/solidDoWant/githubgate/pull/292))
- An authorization ceiling and a user rule accept only operations on `/user` paths. Before, an operation on a repository, organization, team or App path with an unusual parameter name could pass as a user operation. ([#295](https://github.com/solidDoWant/githubgate/pull/295))

Internal work: the design of user access tokens, the fake GitHub for the device flow, single-use refresh tokens and SSH signing keys, e2e and live tests for user authorizations, an update of `golang.org/x/oauth2`, and CI changes for CPU and memory limits, CPU pinning, parallel integration tests, timeouts, the Go build cache and kind diagnostics ([#250](https://github.com/solidDoWant/githubgate/pull/250), [#262](https://github.com/solidDoWant/githubgate/pull/262), [#263](https://github.com/solidDoWant/githubgate/pull/263), [#265](https://github.com/solidDoWant/githubgate/pull/265), [#268](https://github.com/solidDoWant/githubgate/pull/268), [#270](https://github.com/solidDoWant/githubgate/pull/270), [#274](https://github.com/solidDoWant/githubgate/pull/274), [#276](https://github.com/solidDoWant/githubgate/pull/276), [#278](https://github.com/solidDoWant/githubgate/pull/278), [#279](https://github.com/solidDoWant/githubgate/pull/279), [#284](https://github.com/solidDoWant/githubgate/pull/284), [#285](https://github.com/solidDoWant/githubgate/pull/285), [#296](https://github.com/solidDoWant/githubgate/pull/296), [#302](https://github.com/solidDoWant/githubgate/pull/302), [#303](https://github.com/solidDoWant/githubgate/pull/303), [#305](https://github.com/solidDoWant/githubgate/pull/305)).

### Known issues

- A refresh token works only once. If githubgate stops after GitHub returns a new token pair and before githubgate writes it to the Secret, the new pair is lost. The next refresh then ends in `RefreshTokenRevoked`, and you must authorize again. This is rare. ([#271](https://github.com/solidDoWant/githubgate/pull/271))
- If the leader replica changes during a device flow, the flow is lost and the authorization reports `FlowLost`. Set the annotation again. ([#266](https://github.com/solidDoWant/githubgate/pull/266))

## 0.7.0-rc.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release is a candidate (a test version before the final release) for 0.7.0. It adds user access tokens. A user access token is a token of a GitHub App that acts as one GitHub user. With it, a workload in proxy mode can use resources that belong to a user account, for example SSH signing keys. The workload never sees the token. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.7.0-rc.1 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- If you install the CRDs without the chart, apply the new `crds.yaml` of this release. It now has four CRDs. The new CRD is `GitHubUserAuthorization`. The chart installs it for you. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- If you manage the RBAC of githubgate yourself, let githubgate create, update and patch Secrets in the admin namespaces. Also let it read and watch `githubuserauthorizations` and write their status. githubgate keeps the tokens of a user authorization in a Secret that it creates. The chart sets these permissions for you. Secrets in other namespaces stay closed to githubgate. ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#285](https://github.com/solidDoWant/githubgate/pull/285))
- If you want to use user access tokens, change the settings of your GitHub App first. Add the account permission that you need, for example `SSH signing keys: Read and write`. Turn on `Enable Device Flow`. Keep `Expire user authorization tokens` on. Then give githubgate the client ID of the App in `clientID` or `clientIDSecretRef` of the `GitHubApp`. The steps are in [Set up user access tokens](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/user-access-setup.md). ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#266](https://github.com/solidDoWant/githubgate/pull/266), [#286](https://github.com/solidDoWant/githubgate/pull/286))

Apps, installations, policies, chart values and clients of 0.6.4 work without a change. You do not need user access tokens to upgrade.

### User access tokens

- The new kind `GitHubUserAuthorization` lives in an admin namespace. It names an App, the GitHub user (`login`) who must authorize the App, a ceiling of REST operations, and the namespaces that can use it. The ceiling is the most that any policy can call as this user. It accepts only operations on `/user` paths. ([#264](https://github.com/solidDoWant/githubgate/pull/264), [#295](https://github.com/solidDoWant/githubgate/pull/295))
- A `GitHubApp` has two new fields, `clientID` and `clientIDSecretRef`, for the client ID of the App. Set at most one. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- You authorize the App as the user with GitHub's device flow. You start the flow when you set the annotation `githubgate.soliddowant.dev/authorize` to a new value. The status shows the code and the address where the user enters it. githubgate stores the tokens in the Secret `NAME-user-token`, where `NAME` is the name of the authorization. If a different user authorizes the App, githubgate discards the token and reports `WrongUser`. ([#266](https://github.com/solidDoWant/githubgate/pull/266))
- The leader replica refreshes the user access token before it expires. A refresh token works only once, so githubgate writes each new pair to the Secret at once. If GitHub refuses the refresh token, the authorization reports `RefreshTokenRevoked` and you authorize again. If GitHub refuses a user access token with `401`, the leader refreshes it at once. ([#271](https://github.com/solidDoWant/githubgate/pull/271), [#283](https://github.com/solidDoWant/githubgate/pull/283), [#298](https://github.com/solidDoWant/githubgate/pull/298))
- The conditions of a `GitHubUserAuthorization` tell you what to do, for example `AuthorizationRequired`, `AuthorizationExpired`, `DeviceFlowDisabled` or `SpecChanged`. If you change the App, the client ID or the login after you authorized, githubgate clears the stored tokens and you authorize again. If a new authorization fails while a token is stored, the conditions show the failure. ([#266](https://github.com/solidDoWant/githubgate/pull/266), [#293](https://github.com/solidDoWant/githubgate/pull/293), [#297](https://github.com/solidDoWant/githubgate/pull/297))
- New metrics: `githubgate_user_authorization_ready`, `githubgate_user_token_refreshes_total` and `githubgate_user_refresh_token_expiry_seconds`. Only the leader replica reports them. ([#271](https://github.com/solidDoWant/githubgate/pull/271))

### Access policies

- A policy rule can now have `userAuthorizationRef` in place of `installationRef`. This is a user rule. It works in proxy mode only, and it lists exact operations in `proxy.allowOperations`. The authorization must list each operation in its ceiling and must admit the namespace of the policy. Otherwise the rule is not accepted, and the status of the policy says why. ([#264](https://github.com/solidDoWant/githubgate/pull/264))
- In proxy mode, githubgate sends a REST request that a user rule allows with the user access token. A request that no user rule lists never gets a user token. If the token is missing, expired or revoked, the caller gets `503`, and githubgate never sends the request with an installation token. The audit line records `access: "user"`, the authorization and the login, but never the token. The steps for workload users are in [Use a user rule](https://github.com/solidDoWant/githubgate/blob/master/docs/user/user-rules.md). ([#281](https://github.com/solidDoWant/githubgate/pull/281), [#286](https://github.com/solidDoWant/githubgate/pull/286))

### Deployment

- New chart value `githubOAuthURL` and server flag `--github-oauth-url`. They set the host of the device flow. The default is `https://github.com`. Set them only for GitHub Enterprise Server or a test double. ([#266](https://github.com/solidDoWant/githubgate/pull/266), [#285](https://github.com/solidDoWant/githubgate/pull/285))

### Security

- githubgate never follows a redirect on a call to the OAuth endpoints of GitHub. A redirect cannot send a refresh token or a device code to a different host. ([#292](https://github.com/solidDoWant/githubgate/pull/292))
- An authorization ceiling and a user rule accept only operations on `/user` paths. Before, an operation on a repository, organization, team or App path with an unusual parameter name could pass as a user operation. ([#295](https://github.com/solidDoWant/githubgate/pull/295))

Internal work: the design of user access tokens, the fake GitHub for the device flow, single-use refresh tokens and SSH signing keys, e2e and live tests for user authorizations, an update of `golang.org/x/oauth2`, and CI changes for CPU and memory limits, timeouts, the Go build cache and kind diagnostics ([#250](https://github.com/solidDoWant/githubgate/pull/250), [#262](https://github.com/solidDoWant/githubgate/pull/262), [#263](https://github.com/solidDoWant/githubgate/pull/263), [#265](https://github.com/solidDoWant/githubgate/pull/265), [#268](https://github.com/solidDoWant/githubgate/pull/268), [#270](https://github.com/solidDoWant/githubgate/pull/270), [#274](https://github.com/solidDoWant/githubgate/pull/274), [#276](https://github.com/solidDoWant/githubgate/pull/276), [#278](https://github.com/solidDoWant/githubgate/pull/278), [#279](https://github.com/solidDoWant/githubgate/pull/279), [#284](https://github.com/solidDoWant/githubgate/pull/284), [#285](https://github.com/solidDoWant/githubgate/pull/285), [#296](https://github.com/solidDoWant/githubgate/pull/296)).

### Known issues

- A refresh token works only once. If githubgate stops after GitHub returns a new token pair and before githubgate writes it to the Secret, the new pair is lost. The next refresh then ends in `RefreshTokenRevoked`, and you must authorize again. This is rare. ([#271](https://github.com/solidDoWant/githubgate/pull/271))
- If the leader replica changes during a device flow, the flow is lost and the authorization reports `FlowLost`. Set the annotation again. ([#266](https://github.com/solidDoWant/githubgate/pull/266))

## 0.6.4

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release is the first complete release since 0.5.0. It has the same code as the candidate 0.6.4-rc.1 (a test version before the final release). It publishes the changes of 0.6.0 to 0.6.3 with a complete GitHub release. The versions 0.6.0, 0.6.1, 0.6.2 and 0.6.3 have tags in git, but none of them was fully published. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes two problems with `publicRead`. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.4 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- Do not install 0.6.0 or 0.6.2. Their release runs stopped before they published anything, so no images, no chart and no GitHub release exist for them. ([#226](https://github.com/solidDoWant/githubgate/pull/226), [#237](https://github.com/solidDoWant/githubgate/pull/237))
- Upgrade from 0.5.0, 0.6.1 or 0.6.3 to 0.6.4. The images and the charts of 0.6.1 and 0.6.3 work, but these versions have no GitHub release, no `crds.yaml` and no client binaries. 0.6.4 has the same code with all release files. ([#231](https://github.com/solidDoWant/githubgate/pull/231), [#237](https://github.com/solidDoWant/githubgate/pull/237), [#242](https://github.com/solidDoWant/githubgate/pull/242))

Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.4/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

### Fixes

- A `publicRead` rule that names a repository that does not exist, or that the installation cannot reach, no longer breaks every public read of that caller. Before, each public read of that caller failed. ([#227](https://github.com/solidDoWant/githubgate/pull/227))
- When githubgate intercepts a pod, it now also sets `NIX_SSL_CERT_FILE` to the CA bundle, and adds to a value that the pod already has. Tools built with Nix read this variable before `SSL_CERT_FILE`. Before, these tools in an intercepted pod did not trust the certificate of githubgate. ([#245](https://github.com/solidDoWant/githubgate/pull/245))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release. It runs as one task, which creates the GitHub release and uploads its files after it pushes the images and the chart. The release upload and the stage TLS check use the same request code and the same CA bundle. The CI stage containers trust the CA of githubgate's own proxy for every tool, and each CI run makes sure that a stage container can connect to `api.github.com` over verified TLS before any stage starts. The e2e tests print pod diagnostics when a pod does not become ready in time ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231), [#234](https://github.com/solidDoWant/githubgate/pull/234), [#237](https://github.com/solidDoWant/githubgate/pull/237), [#239](https://github.com/solidDoWant/githubgate/pull/239), [#242](https://github.com/solidDoWant/githubgate/pull/242), [#246](https://github.com/solidDoWant/githubgate/pull/246)).

### Known issues

None.

## 0.6.4-rc.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release is a candidate (a test version before the final release) for 0.6.4. It publishes the changes of 0.6.0 to 0.6.3 with a complete GitHub release. The versions 0.6.0, 0.6.1, 0.6.2 and 0.6.3 have tags in git, but none of them was fully published. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes two problems with `publicRead`. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.4-rc.1 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- Do not install 0.6.0 or 0.6.2. Their release runs stopped before they published anything, so no images, no chart and no GitHub release exist for them. ([#226](https://github.com/solidDoWant/githubgate/pull/226), [#237](https://github.com/solidDoWant/githubgate/pull/237))
- Upgrade from 0.5.0, 0.6.1 or 0.6.3 to 0.6.4 when it is released. The images and the charts of 0.6.1 and 0.6.3 work, but these versions have no GitHub release, no `crds.yaml` and no client binaries. 0.6.4 has the same code with all release files. ([#231](https://github.com/solidDoWant/githubgate/pull/231), [#237](https://github.com/solidDoWant/githubgate/pull/237), [#242](https://github.com/solidDoWant/githubgate/pull/242))

Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.4-rc.1/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

### Fixes

- A `publicRead` rule that names a repository that does not exist, or that the installation cannot reach, no longer breaks every public read of that caller. Before, each public read of that caller failed. ([#227](https://github.com/solidDoWant/githubgate/pull/227))
- When githubgate intercepts a pod, it now also sets `NIX_SSL_CERT_FILE` to the CA bundle, and adds to a value that the pod already has. Tools built with Nix read this variable before `SSL_CERT_FILE`. Before, these tools in an intercepted pod did not trust the certificate of githubgate. ([#245](https://github.com/solidDoWant/githubgate/pull/245))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release. It runs as one task, which creates the GitHub release and uploads its files after it pushes the images and the chart. The release upload and the stage TLS check use the same request code and the same CA bundle. The CI stage containers trust the CA of githubgate's own proxy for every tool, and each CI run makes sure that a stage container can connect to `api.github.com` over verified TLS before any stage starts. The e2e tests print pod diagnostics when a pod does not become ready in time ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231), [#234](https://github.com/solidDoWant/githubgate/pull/234), [#237](https://github.com/solidDoWant/githubgate/pull/237), [#239](https://github.com/solidDoWant/githubgate/pull/239), [#242](https://github.com/solidDoWant/githubgate/pull/242), [#246](https://github.com/solidDoWant/githubgate/pull/246)).

### Known issues

None.

## 0.6.3

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release publishes the changes of 0.6.0, 0.6.1 and 0.6.2 with a complete GitHub release. The versions 0.6.0, 0.6.1 and 0.6.2 have tags in git, but none of them was fully published. Use 0.6.3 instead. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes two problems with `publicRead`. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.3 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- Do not install 0.6.0 or 0.6.2. Their release runs stopped before they published anything, so no images, no chart and no GitHub release exist for them. ([#226](https://github.com/solidDoWant/githubgate/pull/226), [#237](https://github.com/solidDoWant/githubgate/pull/237))
- Upgrade from 0.5.0 or 0.6.1 to 0.6.3. The images and the chart of 0.6.1 work, but 0.6.1 has no GitHub release, no `crds.yaml` and no client binaries. 0.6.3 has the same code with all release files. ([#231](https://github.com/solidDoWant/githubgate/pull/231), [#237](https://github.com/solidDoWant/githubgate/pull/237))

Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.3/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

### Fixes

- A `publicRead` rule that names a repository that does not exist, or that the installation cannot reach, no longer breaks every public read of that caller. Before, each public read of that caller failed. ([#227](https://github.com/solidDoWant/githubgate/pull/227))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release. It runs as one task, which creates the GitHub release and uploads its files after it pushes the images and the chart. The CI stages trust the CA of githubgate's own proxy in real CI runs. Each CI run makes sure that a stage container can connect to `api.github.com` over verified TLS before any stage starts, and any HTTP response over verified TLS now passes that check. The e2e tests print pod diagnostics when a pod does not become ready in time ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231), [#234](https://github.com/solidDoWant/githubgate/pull/234), [#237](https://github.com/solidDoWant/githubgate/pull/237), [#239](https://github.com/solidDoWant/githubgate/pull/239)).

### Known issues

None.

## 0.6.2

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release publishes the changes of 0.6.0 and 0.6.1 with a complete GitHub release. We tagged 0.6.0, but its release run stopped before it published anything. 0.6.1 published the images and the Helm chart, but its release run could not create the GitHub release or upload its files. Use 0.6.2 instead. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes two problems with `publicRead`. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.2 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- Do not install 0.6.0. The version 0.6.0 has a tag in git, but nothing of it was published. ([#226](https://github.com/solidDoWant/githubgate/pull/226))
- Upgrade from 0.5.0 or 0.6.1 to 0.6.2. The images and the chart of 0.6.1 work, but 0.6.1 has no GitHub release, no `crds.yaml` and no client binaries. 0.6.2 has the same code with all release files. ([#231](https://github.com/solidDoWant/githubgate/pull/231))

Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.2/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

### Fixes

- A `publicRead` rule that names a repository that does not exist, or that the installation cannot reach, no longer breaks every public read of that caller. Before, each public read of that caller failed. ([#227](https://github.com/solidDoWant/githubgate/pull/227))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release. It runs as one task, which creates the GitHub release and uploads its files after it pushes the images and the chart. The CI stages now trust the CA of githubgate's own proxy in real CI runs, so the upload to GitHub works. Each CI run now makes sure that a stage container can connect to `api.github.com` over verified TLS before any stage starts ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226), [#231](https://github.com/solidDoWant/githubgate/pull/231), [#234](https://github.com/solidDoWant/githubgate/pull/234)).

### Known issues

None.

## 0.6.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release publishes the changes of 0.6.0. We tagged 0.6.0, but its release run stopped before it published anything. No images, no chart and no GitHub release of 0.6.0 exist. Use 0.6.1 instead. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes two problems with `publicRead`. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.1 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

- Do not install 0.6.0. Upgrade from 0.5.0 to 0.6.1. The version 0.6.0 has a tag in git, but nothing of it was published. ([#226](https://github.com/solidDoWant/githubgate/pull/226))

Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.1/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

### Fixes

- A `publicRead` rule that names a repository that does not exist, or that the installation cannot reach, no longer breaks every public read of that caller. Before, each public read of that caller failed. ([#227](https://github.com/solidDoWant/githubgate/pull/227))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release. It now runs as one task, which creates the GitHub release and uploads its files after it pushes the images and the chart ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222), [#226](https://github.com/solidDoWant/githubgate/pull/226)).

### Known issues

None.

## 0.6.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release adds files to each GitHub release: the CRDs in one file and the `githubgate-client` binaries. It also fixes one case where `publicRead` sent a request that the policy rules did not allow. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.6.0 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

None. Policies, chart values and clients of 0.5.0 work without a change.

### Deployment

- Each GitHub release now has the file `crds.yaml`. It contains all the CRDs of githubgate. You can install them with `kubectl apply -f https://github.com/solidDoWant/githubgate/releases/download/v0.6.0/crds.yaml`. ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222))

### Client (`git`, `gh`)

- Each GitHub release now has the `githubgate-client` binaries for Linux and macOS, on amd64 and arm64, and the file `checksums.txt` with their SHA-256 sums. You can download the client from the release page instead of copying it from the image. The steps are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222))

### Security

- With `publicRead`, read-only requests for the paths `/orgs/OWNER/...` and `/users/OWNER/...` of the installation's own owner now follow the policy rules. Before, they went out as public reads, also when no rule allowed them. For example, `gh api orgs/OWNER/repos` now gets `403` unless a rule allows it. Requests for other owners do not change. ([#223](https://github.com/solidDoWant/githubgate/pull/223))

Internal work: the release pipeline builds the release files and attaches them to the GitHub release, and its tasks share one volume ([#218](https://github.com/solidDoWant/githubgate/pull/218), [#222](https://github.com/solidDoWant/githubgate/pull/222)).

### Known issues

- If a `publicRead` rule names a repository that does not exist or that the installation cannot reach, every public read of that caller fails. Remove or correct that repository name in the rule. A fix is in progress. ([#221](https://github.com/solidDoWant/githubgate/issues/221))

## 0.5.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release lets the proxy send two more kinds of requests, if a policy turns them on: reads of public repositories of other owners, and downloads from `github.com` web pages such as release assets. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.5.0 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

None. Policies, chart values and clients of 0.4.0 work without a change. The new settings are off until a policy turns them on. `helm upgrade` adds the new fields to the `GitHubAccessPolicy` CRD.

### Access policies

- A rule can set `publicRead: true` in its `proxy` block. Then read-only requests for repositories of other owners go through the proxy, for example `git clone`, `git fetch`, `gh api repos/OWNER/REPO` and nix flake inputs. Read-only API requests that name no repository also go through, for example users, search and the rate limit. githubgate sends these requests with an installation token that has only `metadata: read`. This token reads public data and no private contents. A private repository of another owner gets `404` from GitHub. A write to a repository outside your installation gets `403`, and the error names the repository. A rule with `publicRead` must name at least one repository without a `*`. GraphQL requests do not use `publicRead`. The details are in [Public repositories and web pages](https://github.com/solidDoWant/githubgate/blob/master/docs/user/interception.md#public-repositories-and-web-pages). ([#215](https://github.com/solidDoWant/githubgate/pull/215))
- A rule can set `webPaths: true` in its `proxy` block. Then `GET` and `HEAD` requests on `github.com` that are not git go through the proxy, for example `curl -L https://github.com/OWNER/REPO/releases/latest/download/ASSET`. githubgate sends them with no credential and returns redirects to you unchanged. Without this setting, these requests still get `404`. ([#215](https://github.com/solidDoWant/githubgate/pull/215))

### Audit log

- The proxy audit line has a new field, `access`. It tells how githubgate sent an allowed request: `rules` (a token of the rules), `public_read` (the token for public data, and `repositories` names the repository that the caller read) or `web` (no credential). The field is empty when githubgate refused the request. ([#215](https://github.com/solidDoWant/githubgate/pull/215))

### Known issues

None.

## 0.4.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release adds a new mode to the client: a pod that uses interception can now publish commits that GitHub signs. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.4.0 --namespace githubgate --create-namespace
```

### Upgrade notes for admins

None. Policies, chart values and clients of 0.3.1 work without a change.

### Client (`git`, `gh`)

- `githubgate-client push --intercepted` publishes your local commits so that GitHub signs them, in a pod with [interception](https://github.com/solidDoWant/githubgate/blob/master/docs/user/interception.md). The client sends its requests to `api.github.com` with no token, and the interceptor adds the ServiceAccount token of the pod. Your rule needs `contents: write`, and the branch must match its `pushRefs`. You cannot use `--intercepted` together with `--proxy`. Use it only in a pod that interception changed, because in other pods GitHub refuses the request. The details are in [the client](https://github.com/solidDoWant/githubgate/blob/master/docs/user/client.md). ([#212](https://github.com/solidDoWant/githubgate/pull/212))

### Fixes

- When the interceptor stops, it now waits for all of its work to end before it exits. Before, one part of it could still run for a short time after the stop. ([#208](https://github.com/solidDoWant/githubgate/pull/208))

Internal work: CI gets its GitHub access from githubgate, through the interceptor, and a release now creates its GitHub release itself ([#209](https://github.com/solidDoWant/githubgate/pull/209), [#210](https://github.com/solidDoWant/githubgate/pull/210)).

### Known issues

None.

## 0.3.1

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This patch release changes how the Helm chart gets its TLS certificates. The chart no longer makes a CA key itself. cert-manager makes every certificate, or you give each certificate as a Secret. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.3.1 --namespace githubgate --create-namespace
```

The certificate setup is in [Certificates](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/token-mode-setup.md#certificates).

### Upgrade notes for admins

- Give the token API's CA to your clients again. The chart no longer creates the ConfigMap `githubgate-ca` by default. With cert-manager, the CA is in the Secret `<fullname>-ca`, key `ca.crt`, in the githubgate namespace. Copy it to your client namespaces, and restart the clients that read the old CA. The new CA is not the CA of 0.3.0. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- If you want the ConfigMap, set `certificate.caConfigMap.create: true` and put the CA in `certificate.caBundle`. The chart fills the ConfigMap only from that value. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- Rename your chart value `certificate.caConfigMapName` to `certificate.caConfigMap.name`. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- Replace your chart value `certificate.caDurationDays` with `certificate.caDuration`, as a duration such as `87600h`. cert-manager reads durations in this form. ([#205](https://github.com/solidDoWant/githubgate/pull/205))

### Deployment

- The chart reads every certificate from a Secret. With `certManager.enabled: true` (the default), cert-manager makes a CA and the certificates. The CA can sign only the names of the githubgate Service. No private key is in the rendered manifests or in the Helm release record. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- You can give your own certificates. Set `certManager.enabled: false` and name a Secret in `tls.existingSecret`, `webhook.existingSecret` and `proxy.existingSecret`, for each part that you turn on. Each value also works with cert-manager on, and it then replaces only that one certificate. If a Secret that githubgate needs is missing, the chart does not install. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- The API server must trust the webhooks. Without cert-manager, set `webhook.caBundle` or add your own annotation in `webhook.annotations`, for example `cert-manager.io/inject-ca-from`. If you set neither, the chart does not install. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- `inject.proxyCAConfigMap` is now optional. If it is empty, the interceptor trusts the `ca.crt` of the proxy certificate's Secret. ([#205](https://github.com/solidDoWant/githubgate/pull/205))
- The images and the chart on ghcr.io now link to this repository. ([#203](https://github.com/solidDoWant/githubgate/pull/203))

### Security

- `github.com/google/go-github` is now v93. This version of the library sends credentials only to the API origins that it is set up for. ([#202](https://github.com/solidDoWant/githubgate/pull/202))

Internal work: the end-to-end tests use the new certificate setup.

### Known issues

None.

## 0.3.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release covers phase 3 of the project: transparent interception. With interception, a pod needs no GitHub setup. githubgate adds a helper container, the interceptor, to each pod that opts in. `git`, `gh` and `curl` in the pod then work as they do against GitHub, and every request goes through the githubgate proxy. The pod never holds a GitHub token. A KubeVirt VM can use the interceptor in its launcher pod. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.3.0 --namespace githubgate --create-namespace
```

The setup is in [Set up interception](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/interception-setup.md). Workload users read [Use GitHub with no setup in your pod](https://github.com/solidDoWant/githubgate/blob/master/docs/user/interception.md).

### Upgrade notes for admins

None. Policies, chart values and clients of 0.2.0 work without a change. Interception is off until you turn it on with the chart value `inject.enabled`.

### Interception

- An injection webhook adds the interceptor to each pod in a namespace with the label `githubgate.soliddowant.dev/inject=true`. One pod opts out with the annotation `githubgate.soliddowant.dev/inject: "false"`. In other namespaces, one pod opts in with the label `githubgate.soliddowant.dev/inject: "true"`. ([#149](https://github.com/solidDoWant/githubgate/pull/149), [#173](https://github.com/solidDoWant/githubgate/pull/173))
- The webhook points `github.com`, `api.github.com` and `uploads.github.com` to the interceptor, at `127.43.0.1:443` by default. It sets the trust variables (`SSL_CERT_FILE` and others) in every container, so that the tools trust the interceptor. It sets `GH_TOKEN` to a placeholder when a container has no GitHub token variable, because `gh` does not run without one. ([#149](https://github.com/solidDoWant/githubgate/pull/149), [#173](https://github.com/solidDoWant/githubgate/pull/173))
- If a container sets an egress proxy variable, the webhook adds the three GitHub names to `NO_PROXY` and `no_proxy`. It keeps the values that the container sets, also when they come from `envFrom`. ([#149](https://github.com/solidDoWant/githubgate/pull/149), [#199](https://github.com/solidDoWant/githubgate/pull/199))
- If a container sets a CA variable to a file in one of its volumes, githubgate joins that file with the CA of the interceptor. Other hosts keep the CAs of the file. A namespace can add its own private CAs with the annotation `githubgate.soliddowant.dev/extra-ca-configmap`. ([#160](https://github.com/solidDoWant/githubgate/pull/160))
- The webhook rejects a pod that it cannot make safe, and the error gives the reason. Examples are `hostNetwork: true`, a container on port 443, or a CA variable that points to a file in the image. ([#149](https://github.com/solidDoWant/githubgate/pull/149), [#160](https://github.com/solidDoWant/githubgate/pull/160))
- The pod annotation `githubgate.soliddowant.dev/listen` sets another address for the interceptor. A KubeVirt VM uses it to reach the interceptor in its launcher pod at its gateway address, `10.0.2.1:443`. Nothing of githubgate runs in the VM, and the ServiceAccount token never enters the VM. ([#173](https://github.com/solidDoWant/githubgate/pull/173), [#193](https://github.com/solidDoWant/githubgate/pull/193))

### Client (`git`, `gh`)

- `githubgate-client intercept` runs the interceptor. It removes every credential that your tools send. Then it sends the request to the githubgate proxy with the ServiceAccount token of the pod. It makes its own CA for each pod and writes the trust files that your containers read. ([#151](https://github.com/solidDoWant/githubgate/pull/151))
- The interceptor stops cleanly and exits 0 when the pod stops it. ([#195](https://github.com/solidDoWant/githubgate/pull/195))

### Deployment

- The chart values `inject.enabled`, `inject.image.digest`, `inject.proxyAddress`, `inject.proxyCAConfigMap` and `inject.extraCAConfigMap` install and set up the injection webhook. The webhook needs cert-manager and proxy mode (`proxy.enabled`). ([#150](https://github.com/solidDoWant/githubgate/pull/150))
- The webhook fails closed. If it is not available or has no proxy CA, githubgate refuses to create pods that opt in. It never lets such a pod through without the interceptor. ([#149](https://github.com/solidDoWant/githubgate/pull/149), [#150](https://github.com/solidDoWant/githubgate/pull/150))
- The githubgate pods check their readiness every second, so a new pod gets requests sooner. ([#158](https://github.com/solidDoWant/githubgate/pull/158))
- The guides for interception are under `docs/user/` and `docs/admin/`. ([#165](https://github.com/solidDoWant/githubgate/pull/165))

### Security

- The interceptor and the webhook refuse a listen address that covers every address of the pod, also when it is written as `::ffff:0.0.0.0`. On such an address, other pods can reach the interceptor and send requests with the token of your pod. ([#196](https://github.com/solidDoWant/githubgate/pull/196))
- `golang.org/x/sys` is now v0.50.0. ([#184](https://github.com/solidDoWant/githubgate/pull/184))

### Fixes

- githubgate closes all of its listeners before it stops. ([#180](https://github.com/solidDoWant/githubgate/pull/180))

Internal work: end-to-end tests for interception on kind, a live test on github.com, faster CI (fuzz tests in their own stage, parallel e2e setup), agent tooling and design documents.

### Known issues

None.

## 0.2.0

githubgate gives Kubernetes workloads limited GitHub access through GitHub Apps. A workload never gets the private key of an App. It proves who it is with the token of its ServiceAccount. This release covers phase 2 of the project: proxy mode and signed commits. In proxy mode, the workload gets no GitHub token at all. githubgate forwards each request of the workload to GitHub itself, and it can limit which branches and tags a push changes. Install or upgrade with the Helm chart:

```
helm upgrade --install githubgate oci://ghcr.io/soliddowant/charts/githubgate --version 0.2.0 --namespace githubgate --create-namespace
```

The setup of proxy mode is in [Set up proxy mode](https://github.com/solidDoWant/githubgate/blob/master/docs/admin/proxy-mode-setup.md). Workload users read [Proxy mode](https://github.com/solidDoWant/githubgate/blob/master/docs/user/proxy-mode.md).

### Upgrade notes for admins

None. Policies, chart values and clients of 0.1.0 work without a change. Proxy mode is off until you turn it on with the chart value `proxy.enabled`.

### Proxy mode

- githubgate can serve proxy mode on a second TLS listener for `api.github.com`, `uploads.github.com` and `github.com`. The pod sends its requests there, and githubgate checks the ServiceAccount token of the caller. ([#95](https://github.com/solidDoWant/githubgate/pull/95))
- githubgate checks each request against the proxy rules of the caller. It forwards an allowed request to GitHub with an installation token, and the token never goes to the pod. ([#99](https://github.com/solidDoWant/githubgate/pull/99), [#91](https://github.com/solidDoWant/githubgate/pull/91))
- githubgate reads each GraphQL request before it forwards it. It refuses batches, persisted queries, subscriptions and requests that it cannot read with one meaning. It forwards a rebuilt copy of the request. ([#94](https://github.com/solidDoWant/githubgate/pull/94), [#120](https://github.com/solidDoWant/githubgate/pull/120))
- `pushRefs` limits the branches and tags that `git push` can change. githubgate refuses a push to other refs, and git shows the reason for each ref. ([#100](https://github.com/solidDoWant/githubgate/pull/100))
- Under `pushRefs`, githubgate also refuses other requests that can change a ref outside the list. It refuses creating a deployment, because GitHub can merge the default branch into the ref of the deployment. It refuses publishing a draft release without `tag_name`, because githubgate cannot check the tag that GitHub then creates. ([#121](https://github.com/solidDoWant/githubgate/pull/121), [#127](https://github.com/solidDoWant/githubgate/pull/127))

### Access policies

- A rule of a `GitHubAccessPolicy` takes a `proxy` block with `enabled`, `pushRefs`, `allowOperations`, `denyOperations`, `allowMutations` and `denyMutations`. A rule must turn on `token.enabled`, `proxy.enabled` or both. Token mode uses only the rules with `token.enabled`. ([#93](https://github.com/solidDoWant/githubgate/pull/93))
- `pushRefs` and the operation and mutation lists need token mode off in the same rule. A caller with a token can go around them. ([#93](https://github.com/solidDoWant/githubgate/pull/93))

### Client (`git`, `gh`)

- `githubgate-client push` publishes your local commits through the GitHub API. GitHub signs the commits, so they show as verified. ([#92](https://github.com/solidDoWant/githubgate/pull/92))
- The client works in proxy mode with `configure --proxy`, `git-credential --proxy`, `gh --proxy` and `push --proxy`. It sends the ServiceAccount token to the githubgate proxy and trusts only the proxy CA in `GITHUBGATE_PROXY_CA_FILE`. ([#104](https://github.com/solidDoWant/githubgate/pull/104))
- `gh --proxy` can download release assets and archives. For this, it needs `GITHUBGATE_PROXY_ADDRESS` (the IP address of the proxy) or `GITHUBGATE_PROXY_HOST` (the name of the proxy Service) in the pod. Without one of them, it stops before it sends the token. ([#126](https://github.com/solidDoWant/githubgate/pull/126))

### Deployment

- The chart values `proxy.enabled`, `proxy.clusterIP` and `proxy.ca` serve proxy mode on the `githubgate-proxy` Service. cert-manager creates the proxy certificate from a CA that you limit to `github.com` and its subdomains. ([#98](https://github.com/solidDoWant/githubgate/pull/98), [#108](https://github.com/solidDoWant/githubgate/pull/108))
- The chart values `proxy.upstreams.*` and `hostAliases`, and the server flags `--proxy-upstream-api`, `--proxy-upstream-uploads`, `--proxy-upstream-git` and `--proxy-upstream-ca-file`, send proxied requests to another GitHub. ([#99](https://github.com/solidDoWant/githubgate/pull/99))
- The guides for proxy mode and signed commits are under `docs/user/` and `docs/admin/`. ([#106](https://github.com/solidDoWant/githubgate/pull/106))

### Security

- A small GraphQL request with deep nesting can no longer make the proxy use hundreds of MiB of memory before the policy check. githubgate refuses a rebuilt request over 1 MiB with `413 too_large`. ([#120](https://github.com/solidDoWant/githubgate/pull/120))
- The proxy closes a connection when no data moves on it for 5 minutes. A pod can no longer use up the proxy with stalled requests. ([#122](https://github.com/solidDoWant/githubgate/pull/122))

### Fixes

- `git push` of more than 1 MiB through the proxy works. Before, the probe request of git failed. ([#117](https://github.com/solidDoWant/githubgate/pull/117))
- `git clone` and `git fetch` through the proxy no longer fail with `415` when git compresses its request. This happened with many branches or tags, or with many local commits. ([#128](https://github.com/solidDoWant/githubgate/pull/128))

Internal work: fake GitHub endpoints for proxy mode and signed commits, end-to-end and live tests, CI, agent tooling and design documents.

### Known issues

None.

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
