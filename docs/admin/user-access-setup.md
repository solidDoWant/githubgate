# Set up user access tokens

For: admins who already run githubgate in proxy mode and want a workload to use GitHub resources that belong to one user account. You know Kubernetes and GitHub Apps. See [Set up proxy mode](proxy-mode-setup.md) first.

Some GitHub resources belong to a user account, not to a repository. Examples are SSH signing keys, GPG keys and emails. GitHub serves them only to a user access token. A user access token is a token of a GitHub App that acts as one user who authorized the App. An installation token gets `403 Resource not accessible by integration` for these resources.

You authorize an App as one user, once, with GitHub's device flow. githubgate keeps the tokens in a Secret and refreshes them. githubgate uses them in proxy mode only, for the REST operations that both your ceiling and the policy of the caller list. A caller never receives the user access token.

## 1. Set up the App

1. In the settings of the GitHub App, add the account permission that you need. For example, add `SSH signing keys: Read and write`. Account permissions apply to the user who authorizes the App.
2. Turn on `Enable Device Flow`.
3. Keep `Expire user authorization tokens` on. githubgate refuses a token that does not expire, and the authorization reports `TokenExpiryDisabled`.
4. Note the client ID of the App. It is on the settings page of the App.
5. Give githubgate the client ID in the `GitHubApp`. Use `clientID` to set it inline:

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubApp
metadata:
  namespace: githubgate
  name: ci-app
spec:
  appIDSecretRef:
    name: ci-app
  privateKeySecretRef:
    name: ci-app
  clientID: Iv1.0123456789abcdef
  allowedNamespaces: {}
```

You can use `clientIDSecretRef` instead, to read the client ID from a Secret in the same namespace. The key is `client-id` by default. Set at most one of the two fields. The client ID is public. An App without a client ID stays Ready, but a `GitHubUserAuthorization` for it reports `InvalidSpec`.

By default, githubgate runs the device flow against `https://github.com`. The server flag `--github-oauth-url` changes that host.

## 2. Create the authorization

Create a `GitHubUserAuthorization` in an admin namespace. It can only live in an admin namespace, like a `GitHubApp`.

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubUserAuthorization
metadata:
  namespace: githubgate
  name: soliddowant-user
spec:
  appRef:
    name: ci-app
  login: solidDoWant
  ceiling:
    operations:
      - users/create-ssh-signing-key-for-authenticated-user
      - users/delete-ssh-signing-key-for-authenticated-user
      - users/list-ssh-signing-keys-for-authenticated-user
  allowedNamespaces:
    names:
      - development
```

- `login` is the GitHub user who must authorize the App. githubgate discards the token of any other user.
- `ceiling.operations` lists the most that any policy can call as this user. Each entry is an exact GitHub REST operation id, with no globs. An empty list allows nothing. githubgate accepts only operations whose path has no owner, repository, organization or enterprise, which in practice means the `/user` endpoints.
- `allowedNamespaces` says which namespaces can refer to this authorization from a policy.

An operation in the ceiling covers every object of that kind on the account of the user. For example, the delete operation can delete any signing key of the user, not only the keys that the caller added. List only the operations that you accept for every such object.

## 3. Authorize

A device flow never starts by itself, so a restart or a failed refresh can never send a code to someone. You start it with an annotation.

1. Set the annotation `githubgate.soliddowant.dev/authorize` to a new value:

```
kubectl -n githubgate annotate githubuserauthorization soliddowant-user githubgate.soliddowant.dev/authorize=$(date +%s) --overwrite
```

2. Read the code and the address from the status:

```
kubectl -n githubgate get githubuserauthorization soliddowant-user -o jsonpath='{.status.authorization}'
```

3. Sign in to GitHub as the user in `spec.login`. Open `verificationURI` and enter the `userCode`.
4. Wait for `Ready` to become `True`.

The code stops working at `status.authorization.expiresAt`. A new annotation value while a flow runs starts the flow again. githubgate keeps the device code in the memory of the leader only. If the leader changes during a flow, the flow is lost and the condition says `FlowLost`. Set the annotation again.

After githubgate gets a token, it calls `GET /user`. If the login is not `spec.login`, githubgate discards the token and reports `WrongUser`.

githubgate stores the tokens in the Secret `soliddowant-user-user-token` in the namespace of the authorization. The name is the name of the authorization with `-user-token` added. The authorization owns the Secret. githubgate refuses to use a Secret of that name that it did not create, and reports `SecretInvalid`.

## 4. Read the conditions

`Ready` is `True` when a valid token is stored and githubgate refreshes it. `AuthorizationRequired` is `True` when you must start or finish the device flow. The reason says why.

| State | `Ready` reason | What to do |
|---|---|---|
| Never authorized | `AuthorizationRequired` | Authorize (step 3). |
| Flow in progress | `AuthorizationPending` | Enter the code at the address in the status. |
| The code expired | `AuthorizationExpired` | Authorize again. |
| The user denied the code | `AuthorizationDenied` | Authorize again, and accept the request. |
| Device flow is off for the App | `DeviceFlowDisabled` | Turn on `Enable Device Flow` in the App. |
| The token does not expire | `TokenExpiryDisabled` | Turn on token expiry in the App, then authorize again. |
| A different user authorized | `WrongUser` | Sign in as `spec.login`, then authorize again. |
| The flow was lost | `FlowLost` | Authorize again. |
| The refresh token was refused | `RefreshTokenRevoked` | Authorize again. |
| The App, the client ID or the login changed after you authorized | `SpecChanged` | Authorize again. githubgate clears the stored token. |
| The client ID is missing | `InvalidSpec` | Set `clientID` or `clientIDSecretRef` in the `GitHubApp`. |
| A Secret is missing or wrong | `SecretUnavailable` or `SecretInvalid` | Fix the Secret. |
| The App is missing, not Ready, or does not admit the namespace | `RefNotFound`, `RefNotReady` or `NotAllowed` | Fix the `GitHubApp`. |

If a refresh fails because GitHub is unreachable, `Ready` stays `True` while the access token is valid. The message says that the refresh is failing. githubgate retries with backoff.

## Refresh and re-authorizing

githubgate refreshes the access token when less than 4 hours of its life remain. Each refresh moves the life of the refresh token to six months, so a running system does not reach that limit. `status.refreshTokenExpiresAt` shows it.

A refresh token works once. githubgate writes the new pair to the Secret as soon as GitHub returns it, and retries the write for up to 2 minutes. If the process stops between the answer of GitHub and a successful write, the new pair is lost. The old refresh token no longer works, and the next refresh ends in `RefreshTokenRevoked`. This limit is rare but real. You fix it with a new authorization.

To re-authorize, set the annotation to a new value again (step 3). The flow runs the same way.

## What a caller sees

If the token is missing, expired or revoked, a request that uses the authorization gets `503`. The message names the authorization. githubgate never sends such a request with an installation token. The audit log records `access: "user"`, the authorization and the login. It never records the token. For the metrics of the authorization, see [the metrics](README.md#the-metrics).

To let a namespace use the authorization, write a user rule. See [Use a user rule](../user/user-rules.md).
