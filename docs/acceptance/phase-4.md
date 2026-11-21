# Phase 4 acceptance checklist: user access tokens

Frozen once approved. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line once they exist. "Live" scenarios are run by hand by the operator against the owner's test App; the others run in CI against the fake GitHub.

"An authorization" below is a `GitHubUserAuthorization` in an admin namespace whose App has a client ID (inline or from a Secret) and the "SSH signing keys" account permission, with device flow on.

## Authorizing

1. Setting the `githubgate.soliddowant.dev/authorize` annotation on an authorization puts a user code, the verification URI and an expiry in its status. Once the user named in `spec.login` approves the code, the authorization becomes Ready and its status shows the login and both expiry times.
    Tests:
2. If a different GitHub user approves the code, no token is stored, and the authorization reports `WrongUser`.
    Tests:
3. A code nobody approves before it expires, a denied code, and an App with device flow off each end in a condition naming the reason. No token is stored, and no new flow starts until the annotation changes again.
    Tests:
4. A flow never starts without a new annotation value: not after a restart, not after a leader change, and not after a failed refresh.
    Tests:
5. The client ID can be given inline or from a Secret (`clientIDSecretRef`, key `client-id` by default). Setting both is rejected. A missing Secret or key is reported on the authorization, and the App stays Ready.
    Tests:

## Using it

6. A pod whose ServiceAccount has a user rule listing `users/create-ssh-signing-key-for-authenticated-user` adds a signing key through the proxy, and then deletes it with a rule listing the delete operation. GitHub receives the user access token, and the pod never sees it.
    Tests:
7. Through the same pod, an operation the rule doesn't list (for example `users/list-emails-for-authenticated-user`), a GraphQL request, a git request and a `github.com` web path never get the user token. They are decided by the installation rules as before.
    Tests:
8. The CRD rejects a user rule that has `installationRef`, `repositories`, `permissions`, an enabled token mode, globs in `allowOperations`, or an empty `allowOperations`. A user rule with an operation outside the authorization's ceiling, or from a namespace it doesn't admit, is not accepted, and its condition names the reason.
    Tests:
9. While the authorization is not Ready, a request its user rule covers gets `503` naming the authorization, and never an installation token.
    Tests:
10. Two user rules of one caller that cover the same operation through different authorizations make that operation's requests fail with `403`.
    Tests:

## Keeping it working

11. The leader refreshes the tokens before the access token reaches half its lifetime, and the new access token is used by every replica. With two replicas, no refresh token is ever used twice.
    Tests:
12. A replica that lost leadership can't overwrite a newer token pair (the Secret update is conditional).
    Tests:
13. If GitHub refuses the refresh token, the authorization reports `RefreshTokenRevoked`, the stored tokens are cleared, and requests get `503` until it is authorized again. A transient GitHub outage during a refresh keeps the authorization Ready while the access token is valid.
    Tests:

## Safety, live and docs

14. No user access token, refresh token or device code appears in any log, event, status, metric, error message or audit line. The leak search covers every token the fake issued.
    Tests:
15. **Live:** on github.com with the test App, the operator authorizes as the owner through the device flow, adds a signing key through the proxy, sees it on the account, deletes it, and sees one refresh succeed.
    Tests:
16. **Docs:** an admin page shows how to set up the App (account permission, device flow, client ID), how to create and authorize a `GitHubUserAuthorization`, what each condition means, and how to re-authorize. The user page shows a user rule. The reference lists the new kind, the new `GitHubApp` fields and the rule field.
    Tests:
