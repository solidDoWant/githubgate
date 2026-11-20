# Phase 3 acceptance checklist: transparent interception

Frozen once approved. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line once they exist. "Live" scenarios are run by hand by the operator against the owner's test App; the others run in CI against the fake GitHub.

"An opted-in namespace" below is a namespace with the injection label from the user guide, in a cluster where githubgate runs with its proxy and injection webhook on.

## Pods

1. In an opted-in namespace, a pod with no githubgate setup of its own (no helper, no token, no CA settings) runs `gh api repos/<owner>/foo` and gets GitHub's answer, when its ServiceAccount's proxy rule covers `foo`.
   Tests:
2. The same pod runs `git clone`, `git fetch` and `git push` (to a ref its rule allows) over HTTPS with no credential helper configured.
   Tests:
3. The same pod fetches `https://api.github.com/repos/<owner>/foo` with `curl` and no token.
   Tests:
4. Whatever credential the pod's tools send (for example a personal access token in `GH_TOKEN`, or one from a credential helper) never reaches the proxy or GitHub; requests are made as the pod's ServiceAccount.
   Tests:
5. From the same pod, `gh release download` and an archive download (both redirect to GitHub's storage hosts) work.
   Tests:
6. A pod in a namespace without the label, or with the opt-out annotation in an opted-in namespace, is created unchanged.
   Tests:
7. A pod in an opted-in namespace that can't be intercepted safely (host network, its own `hostAliases` for the GitHub hosts, or a CA variable pointing at a file githubgate can't read) is rejected with a message naming the reason; a pod whose container sets a CA variable to a file in a mounted volume runs, trusting both its own CA and the pod's.
   Tests:
8. While the injection webhook is unavailable, pods in opted-in namespaces are not created (fail closed), and pods elsewhere are created as usual.
   Tests:
9. The pod's first GitHub request succeeds: the interception is ready before the pod's own containers start.
   Tests:
10. A pod keeps working past its ServiceAccount token's rotation: requests after the projected token is replaced use the new token.
    Tests: `cmd/githubgate-client: TestInterceptSendsTheRotatedServiceAccountTokenOnTheNextRequest`
11. The private key of a pod's interception certificate never appears in any Kubernetes object, event or log.
    Tests:
12. A pod with several containers and init containers (as a Tekton TaskRun pod has) is intercepted in every container that runs after the interception starts.
    Tests:

## Coder VM workspaces

13. The NixOS module in this repository's flake sets up the guest service, the host names and the CA trust, and a VM-less test of the guest service (the same binary and settings in a container) passes scenarios 1–3 against the fake GitHub.
    Tests: `test/e2e: TestTokenModeOnKind/a VM workspace configured by the NixOS module reaches the proxy with gh, git and curl`

## Live and docs

14. **Live:** scenarios 1, 2 and 4 pass on github.com, with the interception, the proxy and the token API in-process.
    Tests: `test/live: TestLiveInterceptionAgainstGitHub`
15. **Docs:** a user page shows how to opt a namespace in and what a pod then gets; an admin page shows how to turn the injection webhook on and how to use the NixOS module in a VM image.
    Tests: `docs: TestUserPageCoversInterceptionOptInAndPodChanges`, `docs: TestAdminPageCoversInjectionWebhookAndNixModule`
