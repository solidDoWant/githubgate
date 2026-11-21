# Phase 4 acceptance checklist: user access tokens

Frozen once approved. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line once they exist. "Live" scenarios are run by hand by the operator against the owner's test App; the others run in CI against the fake GitHub.

"An authorization" below is a `GitHubUserAuthorization` in an admin namespace whose App has a client ID (inline or from a Secret) and the "SSH signing keys" account permission, with device flow on.

## Authorizing

1. Setting the `githubgate.soliddowant.dev/authorize` annotation on an authorization puts a user code, the verification URI and an expiry in its status. Once the user named in `spec.login` approves the code, the authorization becomes Ready and its status shows the login and both expiry times.
    Tests: `internal/controller: TestDeviceFlowAuthorization/the user code is shown, and once the named user approves it the token is stored and the authorization is Ready`
2. If a different GitHub user approves the code, no token is stored, and the authorization reports `WrongUser`.
    Tests: `internal/controller: TestDeviceFlowAuthorization/a code approved by another GitHub user stores nothing and reports WrongUser`
3. A code nobody approves before it expires, a denied code, and an App with device flow off each end in a condition naming the reason. No token is stored, and no new flow starts until the annotation changes again.
    Tests: `internal/controller: TestDeviceFlowExpiredCode`, `internal/controller: TestDeviceFlowAuthorization/a denied code stores nothing and reports AuthorizationDenied`, `internal/controller: TestDeviceFlowAuthorization/an App with the device flow off reports DeviceFlowDisabled and stores nothing`
4. A flow never starts without a new annotation value: not after a restart, not after a leader change, and not after a failed refresh.
    Tests: `internal/controller: TestDeviceFlowAuthorization/no flow starts without an annotation`, `internal/controller: TestDeviceFlowAuthorization/a flow the leader does not hold is reported lost and is not started again`
5. The client ID can be given inline or from a Secret (`clientIDSecretRef`, key `client-id` by default). Setting both is rejected. A missing Secret or key is reported on the authorization, and the App stays Ready.
    Tests: `api/v1alpha1: TestCRDValidation/app with inline client ID`, `api/v1alpha1: TestCRDValidation/app with client ID from a secret`, `api/v1alpha1: TestCRDValidation/app with both client IDs`, `internal/model: TestUserAuthorizationResolution`, `internal/controller: TestStatusOfUserAuthorizations/an authorization whose App has an inline client ID requires authorization`, `internal/controller: TestStatusOfUserAuthorizations/a missing client ID Secret is reported on the authorization and recovers, and the App stays Ready`, `internal/controller: TestStatusOfUserAuthorizations/an App without a client ID makes the authorization an invalid spec, and the App stays Ready`

## Using it

6. A pod whose ServiceAccount has a user rule listing `users/create-ssh-signing-key-for-authenticated-user` adds a signing key through the proxy, and then deletes it with a rule listing the delete operation. GitHub receives the user access token, and the pod never sees it.
    Tests:
7. Through the same pod, an operation the rule doesn't list (for example `users/list-emails-for-authenticated-user`), a GraphQL request, a git request and a `github.com` web path never get the user token. They are decided by the installation rules as before.
    Tests:
8. The CRD rejects a user rule that has `installationRef`, `repositories`, `permissions`, an enabled token mode, globs in `allowOperations`, or an empty `allowOperations`. A user rule with an operation outside the authorization's ceiling, or from a namespace it doesn't admit, is not accepted, and its condition names the reason.
    Tests: `api/v1alpha1: TestCRDValidation/user rule`, `api/v1alpha1: TestCRDValidation/user rule with installationRef`, `api/v1alpha1: TestCRDValidation/user rule with repositories`, `api/v1alpha1: TestCRDValidation/user rule with permissions`, `api/v1alpha1: TestCRDValidation/user rule with token mode enabled`, `api/v1alpha1: TestCRDValidation/user rule with proxy mode disabled`, `api/v1alpha1: TestCRDValidation/user rule without allowOperations`, `api/v1alpha1: TestCRDValidation/user rule with a glob in allowOperations`, `internal/webhook: TestValidate`, `internal/model: TestUserRuleAcceptance`, `internal/controller: TestStatusOfUserAuthorizations/a user rule is accepted only inside the ceiling and from an admitted namespace`
9. While the authorization is not Ready, a request its user rule covers gets `503` naming the authorization, and never an installation token.
    Tests:
10. Two user rules of one caller that cover the same operation through different authorizations make that operation's requests fail with `403`.
    Tests:

## Keeping it working

11. The leader refreshes the tokens before the access token reaches half its lifetime, and the new access token is used by every replica. With two replicas, no refresh token is ever used twice.
    Tests: `internal/controller: TestUserTokenRefresh/the leader refreshes a token with less than four hours left, every replica serves the new token, and no refresh token is used twice`, `internal/controller: TestUserAccessToken`
12. A replica that lost leadership can't overwrite a newer token pair (the Secret update is conditional).
    Tests: `internal/controller: TestUserTokenRefresh/a refresh whose pair was replaced while GitHub answered keeps the newer pair`
13. If GitHub refuses the refresh token, the authorization reports `RefreshTokenRevoked`, the stored tokens are cleared, and requests get `503` until it is authorized again. A transient GitHub outage during a refresh keeps the authorization Ready while the access token is valid.
    Tests: `internal/controller: TestUserTokenRefresh/a refused refresh token clears the stored tokens and reports RefreshTokenRevoked`, `internal/controller: TestUserTokenRefresh/a GitHub outage during a refresh keeps the authorization Ready while the access token is valid, and the refresh is retried`

## Safety, live and docs

14. No user access token, refresh token or device code appears in any log, event, status, metric, error message or audit line. The leak search covers every token the fake issued.
    Tests:
15. **Live:** on github.com with the test App, the operator authorizes as the owner through the device flow, adds a signing key through the proxy, sees it on the account, deletes it, and sees one refresh succeed.
    Tests:
16. **Docs:** an admin page shows how to set up the App (account permission, device flow, client ID), how to create and authorize a `GitHubUserAuthorization`, what each condition means, and how to re-authorize. The user page shows a user rule. The reference lists the new kind, the new `GitHubApp` fields and the rule field.
    Tests:
