# Phase 2 acceptance checklist: proxy mode and signed commits

Frozen once approved. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line once they exist. "Live" scenarios are run by hand by the operator against the owner's test App; the others run in CI against the fake GitHub.

"A proxied pod" below is a pod set up as in the user guide: `hostAliases` sending `api.github.com`, `uploads.github.com` and `github.com` to githubgate's proxy Service, the proxy CA trusted, and its ServiceAccount token sent where a GitHub token would go.

## Proxy

1. A proxied pod whose policy has a proxy rule for repository `foo` reads `foo` through `gh api repos/<owner>/foo` and gets GitHub's answer.
   Tests:
2. Nothing a proxied pod receives (bodies, headers, git output) contains an installation token, across the whole e2e suite.
   Tests:
3. A request with no credential, an invalid ServiceAccount token, or a token with the wrong audience gets `401` and is never sent to GitHub.
   Tests:
4. A request for a repository the caller's proxy rules don't cover gets `403` naming the repository, and is never sent to GitHub.
   Tests:
5. A rule with only `token.enabled` gives nothing through the proxy, and a rule with only `proxy.enabled` gives no token from `/v1/token`.
   Tests:
6. With `denyOperations: [repos/create-release]`, creating a release is refused with `403` naming the operation, while commenting on an issue in the same repository works.
   Tests:
7. With `allowOperations` set, only the listed REST operations work; any other is refused naming the operation.
   Tests:
8. A REST request that matches no operation in GitHub's API description is refused.
   Tests:
9. GraphQL queries work; a mutation in `denyMutations` is refused naming it; `gh pr create` (the `createPullRequest` mutation) works when allowed.
   Tests:
10. A GraphQL request that can't be parsed, is a batch (JSON array), has several operations without `operationName`, or hides mutation fields behind fragments is refused.
    Tests:
11. `git clone` and `git fetch` of an allowed repository work from a proxied pod after `githubgate-client configure --proxy`.
    Tests:
12. With `pushRefs: [refs/heads/agent/**]`, pushing `agent/x` works, while pushing `master` or a tag is refused with git showing githubgate's message naming the ref, and nothing reaches GitHub.
    Tests:
13. Uploading a release asset through `uploads.github.com` follows the same operation rules as the API.
    Tests:
14. Downloading a repository archive (a redirect to GitHub's storage) works from a proxied pod.
    Tests:
15. The proxy serves certificates for exactly the three GitHub hosts; a TLS handshake for any other name fails.
    Tests:
16. Killing one of three replicas during a stream of proxied requests causes no failed request beyond the ones in flight.
    Tests:
17. Every proxied request writes one audit line with namespace, ServiceAccount, repository, operation or mutation, refs for pushes, and the decision, and no token.
    Tests:

## Signed commits

18. `githubgate-client push origin <branch>` publishes the local commits that the remote branch doesn't have; each is created through GitHub's API with the same message, file contents, file modes (executable bit, symlinks) and deletions, authored by the App (GitHub signs only commits the App authors) with the original author kept in a `Co-authored-by:` trailer, and the remote branch ends at the last one.
    Tests:
19. After `push`, the local branch points at the published commits, and the working tree and index are unchanged.
    Tests:
20. A range containing a merge commit, or a change GitHub's API can't express (a submodule), is refused with a message naming the commit, and nothing is published.
    Tests:
21. `push` works in token mode and through the proxy, and through the proxy it obeys `pushRefs`.
    Tests:

## Live and docs

22. **Live:** scenarios 1, 6, 9, 12 and 18 pass on github.com, and the commits from 18 show as Verified.
    Tests:
23. **Docs:** a user page shows how to set up proxy mode in a pod and use `push`; an admin page shows how to set up the proxy CA and write proxy rules.
    Tests:
