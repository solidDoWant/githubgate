# Use a user rule

For: workload users whose admin gave their namespace access to a user account resource, such as SSH signing keys. You know `gh` and your pipeline. See [Use proxy mode and signed commits](proxy-mode.md) first.

A user rule lets a ServiceAccount call a few GitHub REST operations as one user. The call uses a user access token that githubgate holds. You never see the token. User rules work in proxy mode only.

## What you need from your admin

- A `GitHubUserAuthorization` that admits your namespace. Your admin sets it up. See [Set up user access tokens](../admin/user-access-setup.md).
- The name and namespace of the authorization, and the operations it allows.

## Write a rule

This policy lets the `coder-provisioner` ServiceAccount add and delete SSH signing keys, for example for a Coder workspace:

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
      userAuthorizationRef:
        namespace: githubgate
        name: soliddowant-user
      proxy:
        enabled: true
        allowOperations:
          - users/create-ssh-signing-key-for-authenticated-user
          - users/delete-ssh-signing-key-for-authenticated-user
```

A user rule has these limits:

- It has `userAuthorizationRef` and no `installationRef`, `repositories` or `permissions`.
- `proxy.enabled` is `true`. `token` is absent or not enabled, because githubgate never hands out a user access token.
- `allowOperations` lists exact operation ids. The authorization must list every one of them in its ceiling, and it must admit your namespace. Otherwise the rule is not accepted.

The status of the policy shows the `Accepted` condition for each rule.

## How a request is decided

githubgate decides a REST request to `api.github.com` in this order:

1. If one of your accepted user rules lists the operation of the request, those rules alone decide it. githubgate sends the request with the user access token.
2. If your user rules for that operation refer to different authorizations, githubgate refuses the request.
3. If a rule lists the operation but is not accepted, githubgate refuses the request and names the rule and the reason. It does not fall back to an installation rule.
4. If no user rule lists the operation, your installation rules decide as usual. An operation that no user rule lists never gets a user token.

GraphQL, git, `uploads.github.com` and web paths never use a user rule.

## Read a refusal

| Code | Status | Message | What to do |
|---|---|---|---|
| `denied` | 403 | `covered by more than one user authorization` | Ask your admin to remove the overlap. |
| `denied` | 403 | `user rule "<rule>" is not accepted: <reason>` | Fix the rule, or ask your admin to change the ceiling. |
| `unavailable` | 503 | `user authorization <namespace>/<name> is not ready: <reason>` | Ask your admin to authorize again. See the conditions in [Set up user access tokens](../admin/user-access-setup.md#4-read-the-conditions). |

For the other codes, see [Read a refusal](proxy-mode.md#read-a-refusal).
