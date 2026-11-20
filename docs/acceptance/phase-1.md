# Phase 1 acceptance checklist: token mode

Frozen once approved. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line once they exist. "Fake GitHub" scenarios run in CI; "live" ones are run by hand by the operator against the owner's test App.

## Tokens

1. A ServiceAccount named by a policy asks for a token for one allowed repository and gets one whose repositories and permissions (as reported by the fake GitHub) are exactly those the policy gives.
   Tests: `internal/server: TestAllowedRequestReturnsTheTokenWithGitHubsRepositoriesAndPermissions`
2. A ServiceAccount not named by any policy in its namespace is refused (`403`) with a message naming its namespace and ServiceAccount; nothing is minted.
   Tests: `internal/server: TestRefusedRequestsMintNothing/ServiceAccount named by no policy`
3. Asking for a repository the policy doesn't cover is refused, and the message names the repository.
   Tests: `internal/server: TestRefusedRequestsMintNothing/repository outside the policy`, `internal/resolve: TestResolve/a repository the policy doesn't cover is refused, naming the repository`
4. Asking for a permission, or a higher level, than the policy gives is refused, and the message names the permission.
   Tests: `internal/server: TestRefusedRequestsMintNothing/permission above the policy`, `internal/resolve: TestResolve/a higher level than the policy gives is refused, naming the permission`, `internal/resolve: TestResolve/a permission the policy doesn't give is refused, naming the permission`
5. Asking for two repositories with different grants and no permissions gets only the permissions both repositories are given.
   Tests: `internal/resolve: TestResolve/two repositories with different grants and no permissions get only what both are given, at the lower level`, `internal/resolve: TestResolve/repositories that share no permission are refused`, `internal/server: TestRefusedRequestsMintNothing/repositories that share no permission`
6. With no repositories named, the token covers exactly the repositories the caller's rules name exactly; with only glob rules, the request is refused asking for repositories.
   Tests: `internal/resolve: TestResolve/with no repositories named the token covers the repositories the rules name exactly`, `internal/resolve: TestResolve/with no repositories named and only glob rules the request is refused asking for repositories`
7. A PR-pipeline ServiceAccount (`ci-build`, `contents: read`) can clone but can't create a release; the release ServiceAccount (`ci-release`, `contents: write`) in the same namespace can.
   Tests: `internal/resolve: TestResolve/a pull-request ServiceAccount can't get the write access a release needs`, `internal/resolve: TestResolve/the release ServiceAccount in the same namespace gets write access`
8. A policy in namespace `ci-a` that names repository `b` is not accepted when the installation's ceiling doesn't admit it, and `ci-a` gets nothing on `b`.
   Tests: `internal/server: TestRulesThePolicyCannotUseGrantNothing/a rule outside the installation's ceiling grants nothing`, `internal/model: TestRuleAcceptance/a repository outside the ceiling is outside the ceiling`
9. A policy can't use another namespace's ServiceAccount: a token from `ci-b`'s ServiceAccount gets nothing from `ci-a`'s policy.
   Tests: `internal/resolve: TestResolve/a ServiceAccount of another namespace gets nothing from a policy naming the same ServiceAccount name`, `internal/server: TestRulesThePolicyCannotUseGrantNothing/a ServiceAccount of another namespace gets nothing from the policy`
10. A token with the wrong audience, an expired token, or a token of a deleted ServiceAccount (after the 30-second cache) is refused with `401`.
    Tests: `internal/server: TestRefusedRequestsMintNothing/invalid credential`, `internal/server: TestRefusedRequestsMintNothing/no credential`, `internal/auth: TestTokenReviewAuthenticatorRejectsTokensTheAPIServerRefuses/expired token`, `internal/auth: TestTokenReviewAuthenticatorRejectsTokensTheAPIServerRefuses/wrong audience`, `internal/auth: TestTokenReviewAuthenticatorRefusesDeletedServiceAccountAfterCacheExpiry`
11. Two Apps installed on the same owner: a caller allowed through both must name the installation (`409` otherwise); a caller allowed through one gets a token from that one.
    Tests: `internal/resolve: TestResolve/a caller allowed through two installations of one owner must name the installation`, `internal/resolve: TestResolve/a caller allowed through two installations gets a token from the one it names`, `internal/resolve: TestResolve/a caller allowed through one of two installations of an owner gets a token from that one`
12. Repeated requests within the token's lifetime reuse a cached token (one mint at the fake GitHub); a token near expiry is replaced.
    Tests: `internal/server: TestRepeatedRequestsReuseTheCachedTokenUntilItHasUnderTenMinutesLeft`, `internal/server: TestOneReplicaMintsOnceForRepeatedRequests`

## Policies and status

13. A rule outside its installation's ceiling shows `Accepted=False, reason=OutsideCeiling` in the policy's status; the other rules still work.
    Tests: `internal/model: TestRuleOutsideCeilingLeavesOtherRulesInForce`, `internal/server: TestRulesThePolicyCannotUseGrantNothing/a rule inside the ceiling still works when another rule is outside it`, `internal/resolve: TestResolve/a rule outside the ceiling grants nothing while the caller's other rules still work`, `internal/controller: TestStatusOfAppsInstallationsAndPolicies/a policy reports each rule and Ready=False while a rule is not accepted`
14. A policy referencing an installation whose `allowedNamespaces` doesn't admit its namespace shows `NotAllowed` and grants nothing.
    Tests: `internal/model: TestRuleAcceptance/an installation that doesn't admit the policy's namespace is not allowed`, `internal/server: TestRulesThePolicyCannotUseGrantNothing/an installation that doesn't admit the policy's namespace grants nothing`
15. A `GitHubApp` or `GitHubInstallation` created outside the admin namespaces is rejected by the webhook and, with the webhook off, ignored and never used.
    Tests:
16. A `GitHubApp` whose App ID can come inline or from a Secret works either way; one whose key Secret is missing or not a valid RSA key is `Ready=False` with `SecretUnavailable` or `SecretInvalid`; requests through it fail closed with `503`, and fixing the Secret makes it work without a restart.
    Tests: `internal/controller: TestStatusOfAppsInstallationsAndPolicies/an App with a missing or invalid key Secret recovers without a restart`, `internal/controller: TestStatusOfAppsInstallationsAndPolicies/an App ID inline or from a Secret reaches Ready`, `internal/server: TestAppWithABrokenKeySecretFailsClosedUntilTheSecretIsFixed`
17. A `GitHubInstallation` for an owner the App isn't installed on is `Ready=False, reason=InstallationNotFound`.
    Tests: `internal/controller: TestStatusOfAppsInstallationsAndPolicies/an installation on an owner the App is not installed on is InstallationNotFound`
18. Changing a policy (removing a repository) refuses new tokens for that repository within seconds, without a restart.
    Tests: `internal/server: TestRemovedGrantIsRefusedEvenWithACachedToken`, `internal/model: TestWatcherRebuildsTheSnapshotWhenAPolicyChanges`
19. The webhook rejects unknown permission names or levels, and a rule outside its installation's ceiling, with a message naming the field; the CRD rejects a rule with no mode enabled.
    Tests:

## Client

20. With `githubgate-client configure` run, `git clone`, `git fetch` and `git push` of an allowed repository over HTTPS work with no other setup.
    Tests:
21. `git` against a repository the caller isn't allowed fails with git's normal authentication error, and the client prints githubgate's reason on stderr.
    Tests:
22. `githubgate-client gh pr create …` and `gh issue comment …` work in an allowed repository, and still work in a session that has lasted more than one hour.
    Tests:
23. `githubgate-client token --repo foo --permission contents=read` prints a token usable with `curl` against the GitHub API.
    Tests: `cmd/githubgate-client: TestTokenPrintsOnlyTheTokenAndSendsTheRequest`
24. The client reads the ServiceAccount token file on every call, so a rotated token is picked up without restarting anything.
    Tests: `cmd/githubgate-client: TestServiceAccountTokenFileIsReadOnEveryCall`

## Operation

25. The Helm chart installs githubgate with three replicas; killing any one replica during a stream of token requests causes no failed request beyond the one in flight.
    Tests:
26. No key or token appears in logs, events, status, metrics or error messages across the whole e2e suite (checked by searching the collected output for every key and token used).
    Tests:
27. Every request writes one audit line with namespace, ServiceAccount, installation, repositories, permissions and decision.
    Tests: `internal/server: TestEveryRequestWritesOneAuditLineWithoutTheToken`
28. Metrics show requests by decision and the GitHub rate-limit remaining per installation.
    Tests: `internal/server: TestMetricsCountRequestsByDecision`, `internal/github: TestMintUpdatesTheRateLimitGaugeAtMostOnceAMinute`
29. **Live:** against the owner's test App, scenarios 1, 4, 7 and 20 pass on github.com.
    Tests:
30. **Docs:** a user page shows how to use GitHub from a Tekton task and from a Coder workspace; an admin page shows how to create an App, its installation, a ceiling and a policy.
    Tests:
