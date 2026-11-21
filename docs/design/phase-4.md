# Phase 4 design: user access tokens

Implements SPEC.md §4 "User access tokens (phase 4)" for `docs/acceptance/phase-4.md`. Builds on the phase 1–3 designs; everything there still holds unless this file says otherwise. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed. Revised after the design review on #261: the section "Decisions from the design review" at the end overrides anything above it.

## Overview

Some GitHub resources belong to a user account, not to a repository or an installation: SSH signing keys, GPG keys, SSH keys, emails. GitHub serves them only to **user access tokens** (a GitHub App acting as a user who authorized it). Installation tokens get `403 Resource not accessible by integration`. The owner's use: the Coder provisioner adds a per-workspace SSH signing key to the owner's account when a workspace starts and deletes it when it stops, without a personal access token.

Phase 4 lets an admin authorize a GitHub App **as one user**, once, through GitHub's device flow. githubgate keeps the user's tokens in a Secret in the admin namespace, refreshes them, and uses them in **proxy mode only**, for the REST operations an admin ceiling and the caller's policy both list. The user access token never goes to a caller.

```
caller (e.g. Coder provisioner) ──SA token──▶ githubgate proxy ──user access token──▶ api.github.com
                                               (operation in policy ∩ ceiling?)
```

GitHub facts this design relies on (docs.github.com, checked 2026-11-21):
- GitHub Apps have **account (user) permissions**, for example `SSH signing keys` (write covers `POST` and `DELETE /user/ssh_signing_keys`). Installation tokens never get them.
- A user access token "only has permissions that both the user and the app have".
- The **device flow** works for GitHub Apps once enabled in the App's settings (`device_flow_disabled` otherwise). Tokens from the device flow are refreshed without the client secret.
- With expiring tokens (the default), the access token lasts **8 hours** and the refresh token **6 months**. **A refresh token is single-use**: refreshing returns a new access token and a new refresh token, and the old pair stops working.

## Custom resources

### New kind: `GitHubUserAuthorization`

Admin, **admin namespaces only** (like `GitHubApp` and `GitHubInstallation`; the webhook rejects it elsewhere). One object is one App authorized as one user.

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubUserAuthorization
metadata:
  namespace: ci
  name: soliddowant-user
spec:
  appRef:
    name: github            # a GitHubApp; namespace defaults to this object's
  login: solidDoWant        # the GitHub user that must authorize; any other user's token is discarded
  ceiling:
    operations:             # GitHub REST operation ids, exact names (no globs): the most any policy may call as this user
      - users/create-ssh-signing-key-for-authenticated-user
      - users/delete-ssh-signing-key-for-authenticated-user
      - users/list-ssh-signing-keys-for-authenticated-user
  allowedNamespaces:        # who may reference it from a policy (same shape as GitHubInstallation)
    names:
      - development
status:
  observedGeneration: 1
  login: solidDoWant                 # the login the stored token belongs to
  refreshTokenExpiresAt: "2027-05-21T05:00:00Z"
  accessTokenExpiresAt: "2026-11-21T13:00:00Z"
  lastRefreshTime: "2026-11-21T05:00:00Z"
  authorization:                     # only while a device flow is in progress
    userCode: WDJB-MJHT
    verificationURI: https://github.com/login/device
    expiresAt: "2026-11-21T05:15:00Z"
    request: "2026-11-21-1"          # the annotation value this flow answers
  conditions:
    - type: Ready                    # True: a valid token is stored and refreshing
      status: "True"
      reason: Ready
    - type: AuthorizationRequired    # True: no usable token; start or finish the device flow
      status: "False"
      reason: Authorized
```

- **Starting the device flow:** set the annotation `githubgate.soliddowant.dev/authorize` to a new value, for example `kubectl annotate githubuserauthorization soliddowant-user githubgate.soliddowant.dev/authorize=$(date +%s) --overwrite`. The leader then requests a device code and puts the user code, the verification URI and the expiry in `status.authorization`. The admin, signed in to GitHub as `spec.login`, enters the code at the URI. The leader polls at GitHub's interval and stores the tokens. A change to the annotation while a flow runs restarts the flow. A flow never starts without the annotation, so a restart, a new leader or a failed refresh can't send a code to anyone.
- **The device code** (the secret half that the poller trades for a token) stays in the leader's memory only. If the leader changes during a flow, the flow is lost and `AuthorizationRequired` says so; the admin sets the annotation again.
- **Login check:** after getting a token, githubgate calls `GET /user`. If the login isn't `spec.login` (case-insensitive), it discards the token, never stores it, and reports `Ready=False, reason WrongUser`.
- **Ceiling:** exact operation ids only. The webhook rejects any id that isn't a REST operation in githubgate's generated table, any glob, and any GraphQL or git name. An empty ceiling allows nothing.
- **Reasons** added to the shared list: `AuthorizationRequired`, `AuthorizationPending`, `AuthorizationExpired` (the user code expired), `WrongUser`, `RefreshFailed` (transient; the access token is still valid), `RefreshTokenRevoked` (GitHub refused the refresh token: re-authorize).

### Changed: `GitHubApp`

New optional client ID, the App's OAuth client ID from its settings page. It works like the App ID: **at most one** of `clientID` (inline) or `clientIDSecretRef` (a Secret in the same namespace, key `client-id` by default). CEL rejects both together. The value is public, but an admin who keeps the App ID in a Secret will want the client ID next to it. Only a `GitHubUserAuthorization` that references the App needs it. Without it, that object is `Ready=False, reason InvalidSpec`, and the App itself stays Ready. A Secret that is missing or has no such key makes the authorization `SecretUnavailable` / `SecretInvalid`, like the App ID's Secret does for the App. Nothing else changes.

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubApp
metadata:
  namespace: ci
  name: github
spec:
  appIDSecretRef:
    name: github-app
  clientIDSecretRef:          # new; or clientID: Iv23liAbCdEf012345 inline
    name: github-app          # key client-id by default; `key:` overrides it
  privateKeySecretRef:
    name: github-app-key
  allowedNamespaces:
    names:
      - ci
```

### Changed: `GitHubAccessPolicy` rules

A rule references **either** `installationRef` (as today) **or** the new `userAuthorizationRef`. CEL in the CRD enforces, for a rule with `userAuthorizationRef`:
- `installationRef`, `repositories` and `permissions` are absent (the user authorization's operations replace them);
- `token` is absent or `enabled: false` (a user access token is never handed out), and `proxy.enabled` is `true`;
- `proxy.allowOperations` is non-empty and has no globs; `denyOperations`, `allowMutations`, `denyMutations`, `pushRefs`, `publicRead` and `webPaths` are absent.

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata:
  namespace: development
  name: coder-provisioner
spec:
  serviceAccounts:
    - coder-provisioner
  rules:
    - name: signing-keys
      userAuthorizationRef:              # new
        namespace: ci
        name: soliddowant-user
      proxy:
        enabled: true
        allowOperations:
          - users/create-ssh-signing-key-for-authenticated-user
          - users/delete-ssh-signing-key-for-authenticated-user
```

A user rule is **accepted** only if every operation it lists is in the authorization's `ceiling.operations` and the authorization admits the policy's namespace (`OutsideCeiling` / `NotAllowed` otherwise, as for installation rules).

## Deciding a request

In `internal/proxy`, before the installation rules:
1. Classify as today. Only REST requests to `api.github.com` can use a user rule; GraphQL, git, `uploads.github.com` and `github.com` web paths never do.
2. If one or more of the caller's accepted user rules list the request's operation id, the request is decided by those rules alone. If they reference different user authorizations, deny (`403`, "several user authorizations cover this operation"). Otherwise forward with that authorization's current access token. If it has none (`AuthorizationRequired`, expired, revoked), answer `503` with a message naming the authorization, and never fall back to an installation token.
3. Otherwise decide by the installation rules as today. An operation that no user rule lists never gets a user token.

Responses stream back unchanged except for the headers that already get removed. The audit log gets `access: "user"` and the authorization's `namespace/name` and `login`. It never gets the token.

## Storage and refresh

- **Secret:** `<authorization name>-user-token` in the authorization's namespace, owned by the `GitHubUserAuthorization`, type `Opaque`, keys `access-token`, `access-token-expires-at`, `refresh-token`, `refresh-token-expires-at`, `login`. githubgate creates and updates only Secrets that carry the label `githubgate.soliddowant.dev/user-token: "true"` and that the authorization owns, and only in admin namespaces. It refuses to adopt an existing Secret of that name that it didn't create (`SecretInvalid`).
- **Who refreshes:** the leader-elected status controller only. Every replica reads the access token from the Secret through the informer cache, so a refresh reaches every replica within the watch latency.
- **When:** when the access token has less than **4 hours** left (half its lifetime), and at startup if it's already past that. A replica that finds the stored access token expired or about to expire within 5 minutes treats it as missing (step 2 above). It never refreshes on its own.
- **Single-use refresh tokens:** the leader writes the new pair to the Secret **as soon as** GitHub returns it, retrying the update in process with backoff for up to 2 minutes. It uses `resourceVersion` preconditions so a stale leader can't overwrite a newer pair. If the process dies between GitHub's answer and a successful write, the pair is lost: the old one no longer works and the next refresh gets `bad_refresh_token`. That ends in `RefreshTokenRevoked`. The admin re-authorizes, which takes one device-flow round. This is a documented limitation, made rare by the in-process retry.
- **Transient failures** (GitHub unreachable, 5xx) leave `Ready=True` while the access token is still valid, set `RefreshFailed` in the condition message, and retry with backoff. `bad_refresh_token` → `Ready=False, RefreshTokenRevoked`, and the Secret's token keys are cleared.
- **Refresh token lifetime:** each refresh resets it to 6 months, so continuous operation never reaches it. `status.refreshTokenExpiresAt` and a metric show it anyway.

## RBAC

New: `create`, `update` and `patch` on Secrets **in admin namespaces only** (a Role per admin namespace, as for the existing `get/list/watch`), and status updates on the new kind. Nothing cluster-wide. The chart's existing Role template gains the verbs. SPEC §5 changes to match.

## Metrics

- `githubgate_user_authorization_ready{namespace,name}` (0/1)
- `githubgate_user_token_refreshes_total{namespace,name,result}`
- `githubgate_user_refresh_token_expiry_seconds{namespace,name}`

No token, no user code and no login go into labels.

## Fake GitHub

`test/fakegithub` adds:
- the device flow: `POST /login/device/code`, and `POST /login/oauth/access_token` with `grant_type=urn:ietf:params:oauth:grant-type:device_code` that answers `authorization_pending` until a test hook approves the code, `slow_down` when polled too fast, `expired_token` and `access_denied`;
- the refresh grant with **single-use** refresh tokens, as on GitHub;
- `GET /user` and `GET/POST/DELETE /user/ssh_signing_keys`, which accept only user access tokens and answer `403 Resource not accessible by integration` for installation tokens;
- device-flow-disabled (`device_flow_disabled`);
- every user and refresh token it issues, recorded for the leak search.

## Testing

Scenario 11 (concurrent refresh) is an integration test with two managers on one envtest API server and a fake that fails the test if a refresh token is used twice. Scenario 12 (a stale leader) uses `resourceVersion` races. e2e covers 1, 6, 7 and 9 on kind. **Live** (scenario 15) needs a person to enter the user code. The operator runs it with the owner present, against the test App with the "SSH signing keys" account permission and device flow on. It adds one signing key to the owner's account, then deletes it.

## Usable result

The Coder VM template's provisioner (a separate intercepted provisioner Deployment, so coderd itself isn't intercepted) adds and deletes per-workspace SSH signing keys through the proxy. The owner's fine-grained PAT is no longer needed. The infra side is not part of this phase.

## Issue order

1. CRD types, CEL, webhook and status for `GitHubUserAuthorization`, `GitHubApp.clientID`/`clientIDSecretRef` and user rules; the fake's device flow, refresh, `/user` and signing keys (parallel)
2. device flow and storage (needs 1); refresh controller (needs 1)
3. proxy decision and forwarding with user tokens; audit and metrics (needs 2)
4. chart RBAC and values; e2e (needs 3)
5. docs; live (needs 4)

## Decisions from the design review (#261)

The operator accepted every finding. Where this section and the text above differ, this section decides.

### Refresh and storage (H1, H3, M5, M6, L2, L3)

- **One worker per authorization.** The device-flow store, the refresh and the revocation clear for one authorization run in one serialized worker on the leader, never at the same time.
- **Refresh from an uncached read.** Before spending a refresh token, the worker reads the Secret with the manager's API reader (uncached GET), and refreshes only from that copy. It writes the new pair with that copy's `resourceVersion`.
- **On `Conflict`:** re-read. If the stored refresh token is no longer the one that was sent, someone newer wrote: drop the result, and don't record a revocation. Otherwise retry, for up to 2 minutes.
- **The clear is conditional.** On `bad_refresh_token`, clear the token keys only with a conditional update on the `resourceVersion` that held the refused refresh token. If that update conflicts, re-read and decide again.
- **The token is bound to the spec.** The Secret also stores `app` (`namespace/name`) and `client-id`. Serving and refresh use the token only when the stored `login` equals `spec.login` (case-insensitive) and the stored App and client ID equal the current ones, and the Secret has the label and the authorization's owner UID. Otherwise the authorization is `Ready=False, reason SpecChanged`, the token is never used, and the worker clears the token keys (conditionally, as above).
- **Library:** `golang.org/x/oauth2`: `Config.DeviceAuth`, `Config.DeviceAccessToken` (it handles `authorization_pending` and `slow_down`), and a refresh grant through `Config.TokenSource(...).Token()` called once by the worker. Serving **never** uses an auto-refreshing `TokenSource` (`ReuseTokenSource`, `Config.Client`): those would spend the single-use refresh token on any replica. `refresh_token_expires_in` comes from `Token.Extra`.
- **OAuth host flag:** `--github-oauth-url`, default `https://github.com`, for `/login/device/code` and `/login/oauth/access_token`. e2e points it at the fake.
- **Fake quirks to copy:** OAuth errors come with HTTP **200** and an `error` field. Answers are form-encoded unless `Accept: application/json` is sent.
- **Non-expiring tokens are refused.** A token without `expires_in` and `refresh_token` (the App opted out of expiring user tokens) is discarded, with `Ready=False, reason TokenExpiryDisabled`. The admin page says to keep expiry on.
- **A `401` on a user token** is answered `503 unavailable`, with the message naming the authorization. It wakes the worker for an immediate refresh attempt. A successful refresh means the old access token was revoked early; a refused one leads to `RefreshTokenRevoked`.
- **OAuth errors** are reduced to the HTTP status and the OAuth `error` code before they're logged or written to status. `RetrieveError.Error()` can carry the response body, so it's never logged.
- **Names:** CEL caps a `GitHubUserAuthorization` name at 242 characters, so the Secret name fits. `spec.login` must match GitHub's login pattern: alphanumerics and single hyphens, 1–39 characters, not starting or ending with a hyphen.

### Starting a flow (H2)

`status.lastAuthorizeRequest` keeps the last annotation value that was handled, and it stays after the flow ends. The leader writes it **before** requesting a device code. A flow starts only when the annotation is set and differs from that field. A crash after the write loses the flow (fail closed: `AuthorizationRequired`, reason `FlowLost`), and the admin sets the annotation again.

### What a user rule may list (H4, M3)

- The ceiling and a user rule's `allowOperations` accept only REST operations whose path template has **no** `{owner}`, `{repo}`, `{org}` or `{enterprise}` parameter: in practice the `/user…` endpoints. The webhook enforces it, and so does the model's acceptance, so an operations catalogue update makes such a rule `InvalidSpec`.
- An operation listed by **any** of the caller's user rules, accepted or not, is never decided by the installation rules. While the listing rule isn't accepted, its requests get `403 denied` naming the rule and its reason. A user rule stays accepted while its authorization is not Ready (as `RefNotReady` is handled for installations), so scenario 9's `503` is reachable.

### Admission and ownership (M2, M4)

- `GitHubApp.allowedNamespaces` must admit the authorization's namespace (`NotAllowed` otherwise), as for installations.
- The token Secret's owner reference sets `blockOwnerDeletion: false`, so no `finalizers` RBAC is needed. Garbage collection still deletes the Secret with its authorization.

### Conditions (M1)

| State | `Ready` | `AuthorizationRequired` |
|---|---|---|
| never authorized | False, `AuthorizationRequired` | True, `NotAuthorized` |
| flow pending | False, `AuthorizationPending` | True, `AuthorizationPending` |
| code expired | False, `AuthorizationExpired` | True, `AuthorizationExpired` |
| code denied | False, `AuthorizationDenied` | True, `AuthorizationDenied` |
| device flow disabled | False, `DeviceFlowDisabled` | True, `DeviceFlowDisabled` |
| non-expiring token | False, `TokenExpiryDisabled` | True, `TokenExpiryDisabled` |
| wrong user | False, `WrongUser` | True, `WrongUser` |
| flow lost (leader change or crash) | False, `FlowLost` | True, `FlowLost` |
| authorized | True, `Ready` | False, `Authorized` |
| re-authorization failed (`AuthorizationExpired`, `AuthorizationDenied`, `DeviceFlowDisabled`, `TokenExpiryDisabled`, `WrongUser` or `FlowLost`) while a valid token is stored | True, `Ready` (message names the stored login, the request and the failure) | False, the failure reason (same message) |
| refresh failing, access token still valid | True, `Ready` (message says the refresh is failing) | False, `Authorized` |
| refresh token refused | False, `RefreshTokenRevoked` | True, `RefreshTokenRevoked` |
| spec changed after authorizing | False, `SpecChanged` | True, `SpecChanged` |
| client ID missing / Secret missing / Secret invalid | False, `InvalidSpec` / `SecretUnavailable` / `SecretInvalid` | unchanged |
| App not found / not Ready / not admitted | False, `RefNotFound` / `RefNotReady` / `NotAllowed` | unchanged |

A failed re-authorization with a valid stored token keeps the token in use. Its outcome stays until a new flow starts or succeeds; a refresh does not replace it with `Authorized`. With no valid stored token, the rows above apply.

`RefreshFailed` is message text only, not a reason. The refresh metric's `result` label takes the values `ok`, `failed` (transient), `revoked` and `conflict` (dropped because a newer pair was written).

### Error bodies (L1)

These use phase 2's error-body format:
- several authorizations: `403`, `error: denied`, message `githubgate: operation "<id>": covered by more than one user authorization`;
- rule not accepted: `403`, `error: denied`, message `githubgate: operation "<id>": user rule "<rule>" is not accepted: <reason>`;
- not Ready or `401`: `503`, `error: unavailable`, message `githubgate: user authorization <namespace>/<name> is not ready: <reason>`.

### Docs (L4)

The admin page says that an operation grant covers every object of that kind on the user's account. For example, the delete operation can delete any of the user's signing keys, not only the ones the caller added.
