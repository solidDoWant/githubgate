# githubgate specification

githubgate gives Kubernetes workloads limited access to GitHub through GitHub Apps, without ever giving them the App's private key. A workload authenticates with its own Kubernetes ServiceAccount token. Namespaced custom resources declare which GitHub Apps exist, the most each namespace may ever get from an App installation, and what each ServiceAccount may do. githubgate then either **mints a short-lived installation token** limited to those repositories and permissions (token mode), or **holds the token itself and forwards each request** after checking it against finer rules (proxy mode).

This file is the authoritative reference. Each phase is done when its checklist in `docs/acceptance/` passes (see "Phases").

## 1. Purpose and reference behaviour

**The owner's goal.** The owner runs Tekton CI (Pipelines-as-Code, one `ci-<repository>` namespace per repository) and Coder workspaces where autonomous agents work on one repository each. Today workspaces get a broad GitHub token from Coder's external auth, and CI runs get none. The owner wants:
- CI pipelines and agent workspaces never to hold an App private key, and in proxy mode never to see a GitHub token at all;
- access decided by the caller's identity (its ServiceAccount), for example: a PR pipeline (ServiceAccount `ci-build`) may read its repository, while the release pipeline (`ci-release`) may also create releases;
- one GitHub App installed on all the owner's repositories, while a caller in one repository's namespace gets nothing on any other repository;
- policies written as Kubernetes objects in the owner's infra repository and applied by Flux, so a compromised agent that can commit to its own project can't give itself more GitHub access;
- several GitHub Apps, and room to add other ways to authenticate later.

**Reference behaviour.** Token mode does what GitHub's own [installation access token endpoint](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app) does when you pass `repositories` and `permissions`: a token valid for at most one hour, limited to those repositories and permissions. githubgate decides *which* limits a caller may ask for. Proxy mode behaves like `api.github.com`, `uploads.github.com` and `github.com` (git over HTTPS) for the operations a policy allows, and refuses the rest.

**Users:**
- **Admins** install githubgate with Helm and create `GitHubApp` and `GitHubInstallation` objects in the admin namespaces.
- **Chart authors** (the owner's CI chart `cluster/charts/ci/project`, and the Coder workspace setup in the owner's infra repo) create each namespace's `GitHubAccessPolicy`.
- **Workload users** (pipeline scripts, agents, people in workspaces) run `git`, `gh` and the `githubgate` client as they normally would.
- **The owner** is all of these.

## 2. Principles

Non-negotiable. Tests enforce them, and only the owner can change them.

1. **The private key never leaves githubgate, and a token goes only to GitHub.** An App's private key is used only to sign App JWTs sent to GitHub's API. Installation tokens are returned only to the caller they were minted for (token mode) or sent only to GitHub (proxy mode). No key or token is ever logged or put in status, events, metrics or errors.
2. **A caller gets no more than its policy, and a policy no more than its installation's ceiling.** A token or request is allowed only when a `GitHubAccessPolicy` in the caller's own namespace names the caller's ServiceAccount, has an accepted rule covering every repository and permission involved, and that rule is inside the referenced `GitHubInstallation`'s ceiling and admitted by its `allowedNamespaces`. Repository scoping is always also enforced by GitHub, by minting tokens limited to the allowed repositories.
3. **Fail closed.** If githubgate can't decide (token review unavailable, caches not synced, key missing or invalid, GitHub unreachable, rules that disagree), it denies. It never widens a token's repositories or permissions to make a request work.
4. **Ordinary tools keep working.** `git` (through the credential helper) and `gh` (through the client) work as against GitHub, with no change to scripts beyond the documented setup.
5. **Done means the owner's checklist passes.** Nobody raises the bar beyond the checklist and principles 1–4.

## 3. Target environment

- Kubernetes 1.33 or later. The `TokenReview` API must be reachable. Projected ServiceAccount tokens with a custom audience (default `githubgate`).
- GitHub.com only, GitHub Apps only.
- Callers: Tekton task pods (including kata VMs), Coder pod workspaces, and Coder KubeVirt VM workspaces, whose guest gets an audience-bound projected token through a file (the owner's infra repo already does this for toolgate).
- Clients: `git` 2.40+, `gh` 2.60+, `curl`, and anything that sends a GitHub token as `Authorization: token|Bearer …` or as an HTTP Basic password.

## 4. Architecture

One Go binary, `githubgate`, runs the server. A second static binary, `githubgate-client`, is the client (credential helper, token command, `gh` wrapper, signed-commit push).

The server runs several stateless replicas behind a Service. Each replica:
1. serves the token API (and, from phase 2, the proxy) over TLS, plus `/healthz`, `/readyz` and `/metrics` on a separate plain-HTTP port;
2. authenticates each request (see "Authentication");
3. decides from in-memory informer caches of the three kinds and the Secrets they reference (see "Resolution");
4. mints installation tokens from GitHub, caching them per (installation, repositories, permissions) until shortly before they expire;
5. writes one audit log line per request.

A leader-elected controller in the same binary writes status. Serving doesn't need the leader. A replica isn't ready until its caches have synced. Replicas share nothing at runtime except the Kubernetes API and GitHub.

### Custom resources

API group `githubgate.soliddowant.dev`, version `v1alpha1`. All kinds are namespaced and in the category `githubgate`. References and admission follow registrygate's pattern: every reference is `{namespace, name}` (namespace defaults to the referrer's own), and every referenced object has `allowedNamespaces` (`names` list and/or Namespace label `selector`; the object's own namespace is always admitted). A reference that isn't admitted is treated as absent and reported as `NotAllowed`. Every Secret reference is to a Secret in the referring object's own namespace.

| Kind | Who writes it, where | Spec |
|---|---|---|
| `GitHubApp` | admin, **admin namespaces only** | exactly one of `appID` (inline) or `appIDSecretRef` (key `app-id` by default), `privateKeySecretRef` (key `private-key.pem` by default), `allowedNamespaces` (who may create installations of it) |
| `GitHubInstallation` | admin, **admin namespaces only** | `appRef`, `owner` (the user or organisation login the App is installed on, as in `owner/repo`; githubgate finds the installation id), `ceiling` (list of `{repositories, permissions}`: the most any policy may get through it), `allowedNamespaces` (who may reference it from a policy) |
| `GitHubAccessPolicy` | tenant, its own namespace | `serviceAccounts` (names in this namespace), `rules` (below) |

**Admin namespaces** are set by the server flag `--admin-namespaces` (default: its own namespace). `GitHubApp` and `GitHubInstallation` objects elsewhere are ignored and rejected by the webhook, because a ceiling in a tenant's namespace would let the tenant raise it.

**Repositories** are names under the installation's owner (`foo`, not `owner/foo`) and may use `*` globs. **Permissions** are GitHub App permission names with levels `read`, `write` or `admin` (`contents: write`, `pull_requests: write`, …); a higher level contains the lower ones. Only **repository** permissions are accepted: organization and account permissions apply to the whole owner, whatever the token's repositories, so they can't be limited per repository. A ceiling entry gives each permission in it on each repository in it.

**Rules.** `rules` is a list keyed by `name` (`+listType=map`). Each rule has `installationRef`, `repositories`, `permissions`, and one block per mode: `token` and (from phase 2) `proxy`, each with `enabled` (no default). CEL validation in the CRD requires at least one mode enabled, and rejects proxy limits (`proxy.pushRefs`, operation and mutation lists; see "Proxy mode") on a rule that also has `token.enabled: true`, because a token in the caller's hands can't enforce them. Settings for a mode live in its block. A rule is **accepted** only if every (repository pattern, permission, level) it gives is contained in the installation's ceiling (when in doubt, not contained) and the installation admits the policy's namespace. Rules have no order and only ever add access.

**Status.** Every kind has `observedGeneration` and a `Ready` condition with a reason (`Ready`, `InvalidSpec`, `NotAllowed`, `RefNotFound`, `RefNotReady`, `SecretUnavailable`, `SecretInvalid`, `InstallationNotFound`, `InstallationSuspended`, `GitHubUnavailable`, `RulesNotAccepted`); a rule's `Accepted` condition may also be `False` with `OutsideCeiling`. `GitHubApp` status shows the App's slug; `GitHubInstallation` status shows the installation id and whether the ceiling's permissions are within what the App itself was granted. `GitHubAccessPolicy` status has `rules[]` with an `Accepted` condition each. An unaccepted rule is ignored; the rest of the policy still applies.

**Example (what the CI chart would create):**

```yaml
# Admin namespace: one installation covering every repository.
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubInstallation
metadata:
  namespace: githubgate
  name: soliddowant
spec:
  appRef:
    name: ci-app
  owner: solidDoWant
  ceiling:
    - repositories:
        - "*"
      permissions:
        contents: write
        pull_requests: write
        issues: write
        checks: write
        statuses: write
  allowedNamespaces:
    selector:
      matchLabels:
        githubgate.home.arpa/client: "true"
---
# ci-foo: PR builds may only read their own repository.
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata:
  namespace: ci-foo
  name: ci-build
spec:
  serviceAccounts:
    - ci-build
  rules:
    - name: read
      installationRef:
        namespace: githubgate
        name: soliddowant
      repositories:
        - foo
      permissions:
        contents: read
      token:
        enabled: true
---
# ci-foo: the release pipeline may also create releases and tags.
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata:
  namespace: ci-foo
  name: ci-release
spec:
  serviceAccounts:
    - ci-release
  rules:
    - name: release
      installationRef:
        namespace: githubgate
        name: soliddowant
      repositories:
        - foo
      permissions:
        contents: write
      token:
        enabled: true
```

### Authentication

- Callers send a Kubernetes ServiceAccount token, as `Authorization: Bearer`, as `Authorization: token` (what `gh` and many GitHub clients send), or as the password of HTTP Basic auth (what `git` sends; the username is ignored).
- Tokens are checked with `TokenReview` and the configured audience. Definite results are cached for at most 30 seconds, keyed by a SHA-256 of the token.
- Identity is the ServiceAccount's namespace and name, plus the pod name if reported (audit only).
- **Pluggable:** authentication is one interface returning an identity. ServiceAccount tokens are the only implementation now. A later one adds its own policy field (e.g. `subjects`); `serviceAccounts` keeps its meaning.

### Resolution (token mode)

A caller asks for a token for one installation (named directly, or found from the repositories' owner), a set of repositories, and optionally a set of permissions.
1. The candidate rules are the accepted rules with `token.enabled: true`, in policies in the caller's namespace that name its ServiceAccount, for that installation.
2. Each requested repository must match at least one candidate rule. If no permissions were requested, the token gets every permission that **every** requested repository is given (so no repository gets more than its rules give). If permissions were requested, each one must be given on every requested repository.
3. If repositories are omitted, the token covers every repository the caller's rules match by exact name, with the permissions from step 2. Glob-only rules require the caller to name repositories.
4. If the repositories' owner has more than one installation the caller could use (two Apps installed on one owner), the caller must name the installation; otherwise `409`.
5. Anything not covered: `403` with a message naming the namespace, ServiceAccount, repository and permission. Nothing is minted.

The response is the token and its expiry. githubgate never revokes a token it has handed out; a policy change takes effect for new tokens only, and an existing token lives until it expires (at most one hour, GitHub's fixed lifetime).

### Client

`githubgate-client` is a static binary (amd64, arm64), also shipped in a `FROM scratch` image for image-volume mounts. It reads the ServiceAccount token from a file (default `/var/run/secrets/githubgate/token`) fresh on every call. Subcommands:
- `token [--repo …] [--permission name=level …] [--installation ns/name]`: prints a token (token mode).
- `git-credential get|store|erase`: the git credential helper protocol for `https://github.com`; finds the repository from the requested path, asks for a token for it, caches it in memory for the process only.
- `gh …`: runs `gh` with `GH_TOKEN` set to a fresh token, so long-running sessions never use an expired one.
- `configure`: sets up git (`credential.https://github.com.helper`) and, from phase 2, proxy settings in the user's config, keeping every other setting.
- `push` (phase 2, see "Signed commits").

### Proxy mode (phase 2)

githubgate also serves `api.github.com` (REST and GraphQL), `uploads.github.com` and `github.com` (git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack`) under those names, with a serving certificate from a name-constrained CA the admin provides. Clients reach it through `hostAliases` (pods) or `/etc/hosts` (VMs) and trust that CA. The caller sends its ServiceAccount token where it would send a GitHub token; githubgate replaces it with an installation token limited to the caller's allowed repositories and permissions (so GitHub enforces repository scope), and also checks the request against the rule's `proxy` limits:
- **REST operations**, named by GitHub's REST OpenAPI operation ids (e.g. `repos/create-release`), as `allowOperations` / `denyOperations` with globs;
- **GraphQL mutations**, by name, `allowMutations` / `denyMutations` (queries are allowed within the permissions); documents are parsed with a maintained GraphQL parser, and one that can't be parsed is denied;
- **git pushes**: `pushRefs` globs that every ref update in a `git-receive-pack` request must match (e.g. `refs/heads/agent/**`), checked from the pkt-line command list before any pack data is forwarded.

Responses are streamed back unchanged except for headers that would leak the installation token.

### Signed commits (phase 2)

GitHub marks a commit "Verified" when it creates the commit itself through its API with an App's token. `githubgate-client push <remote> <branch>` takes the local commits not yet on the remote branch and re-creates each one through GitHub's API (token or proxy mode), then resets the local branch to the signed commits. Commit ids change; authors and messages are kept, and the committer is the App's bot. Plain `git push` keeps working and stays unsigned. The design must verify, with a live test, which API (GraphQL `createCommitOnBranch` or the Git Database REST API) gives Verified commits while keeping file modes, symlinks and deletions; merge commits may be refused.

### Transparent interception (phase 3)

A native sidecar (`githubgate-client sidecar`) listens on the pod's loopback for the GitHub hosts (via `hostAliases` to `127.0.0.1`), terminates TLS with a certificate from the same CA, replaces whatever credential the client sent (or none) with the pod's current ServiceAccount token, and forwards to githubgate's proxy. Tools then need no wrapper or helper. An admission webhook served by githubgate (opt-in per namespace label) adds the sidecar, `hostAliases`, the projected token and the CA bundle. For Coder VM workspaces the same client runs as a guest service.

### Audit, metrics, admission

- **Audit:** one JSON line per request: time, namespace, ServiceAccount, pod, installation, mode, repositories, permissions, operation (proxy), decision and reason, GitHub status, duration. Never a header value, key or token.
- **Metrics:** request counts by mode, decision and status class; GitHub latency; token cache hits; token review results; GitHub rate-limit remaining per installation (the only object-name label is the installation's `<namespace>/<name>`; admins create few).
- **Admission webhook** (optional, `--webhook`): rejects `GitHubApp`/`GitHubInstallation` outside admin namespaces, unknown permission names or levels, a rule outside its installation's ceiling, and proxy limits on a rule with `token.enabled: true` (also enforced by CEL).

## 5. Security model

- **RBAC:** create `TokenReview`; get/list/watch the three kinds and Namespaces cluster-wide; update their status; get/list/watch Secrets **in admin namespaces only**; leases. Nothing else.
- **Who can change access:** only whoever can write `GitHubAccessPolicy` objects in a namespace (here: Flux from the infra repo). Callers need no Kubernetes RBAC at all. A compromised agent that commits to its own project repository gains nothing, because no policy lives there.
- **Token mode trade-off:** a caller holds a real token for up to one hour, limited by repositories and permissions only. Finer limits (refs, operations) exist only in proxy mode.
- **Threats in scope:** a caller asking for repositories, permissions or installations beyond its policy; a tenant raising its own ceiling; a stolen ServiceAccount token from another audience; replaying a token review result for a deleted ServiceAccount (30 s cache bound); leaking keys or tokens through logs, status, errors or metrics; in proxy mode, smuggling operations through GraphQL batching, aliases or unparseable documents, git pushes to refs outside `pushRefs`, and request paths that parse differently at githubgate and at GitHub.
- **Out of scope:** a compromised githubgate replica or admin namespace; GitHub itself.

## 6. Won't do

- GitHub Enterprise Server; OAuth Apps; user-to-server tokens; personal access tokens.
- Per-caller display names on issues, comments or PRs (GitHub has no such option for Apps), or attribution text added to them.
- Revoking tokens already handed out when a policy changes.
- Fine-grained limits in token mode.
- Re-signing commits sent with plain `git push`.
- Per-caller rate limiting or quotas, and caching GitHub responses.
- Webhooks, GitHub Actions, and creating or installing GitHub Apps.
- Authentication other than ServiceAccount tokens (the interface allows it later).

## 7. Phases

Each phase ends with a release the owner can deploy, and is done when its checklist passes on master and the owner has used the release. Checklists are frozen once approved; only the owner adds to them. The operator writes and approves designs and checklists on the owner's behalf. No spend budget; the measure is checklist scenarios passing.

1. **Token mode** (`docs/acceptance/phase-1.md`): the three kinds with status and webhook, ServiceAccount auth, token API, multiple Apps and installations, HA, the client (`token`, `git-credential`, `gh`, `configure`), Helm chart, audit and metrics, a fake GitHub for tests, and a live test against a real test App. Usable result: CI release pipelines get `contents: write` tokens and PR pipelines read-only ones; workspaces use `git`/`gh` through the client.
2. **Proxy mode and signed commits**: host impersonation for the three GitHub hosts, REST/GraphQL/git-push limits, `push` with Verified commits. Usable result: workspaces push and open PRs without ever seeing a GitHub token, and can't push to protected refs or create releases.
3. **Transparent interception**: the sidecar, the injection webhook, and the VM guest service. Usable result: no wrappers or helpers needed in pods or VMs.

## 8. Testing

- Unit tests with testify table tests; the containment check and resolution get exhaustive table tests.
- A **fake GitHub** (in-repo) implements what githubgate calls: App JWT checks, installation lookup, installation tokens that enforce `repositories` and `permissions` like GitHub (including its error answers), the REST and GraphQL calls the tests make, and git smart HTTP. It must copy GitHub's real quirks; the design lists each one and how it was confirmed.
- Integration tests with envtest; end-to-end tests in kind with the Helm chart, several replicas, and real `git` and `gh`.
- A **live test** (`make test-live`, run by hand by the operator) against a test App the owner creates on a throwaway repository, scheduled early in phase 1. Its private key is never committed, never given to CI or VM agents.
- CI time budget: a PR run takes at most 5 minutes (owner may change).

## 9. Repository layout

`cmd/githubgate`, `cmd/githubgate-client`, `api/v1alpha1`, `internal/…`, `test/fakegithub`, `test/e2e`, `test/live`, `deploy/charts/githubgate`, `docs/`, `scripts/`, Kubebuilder-style codegen (`make generate`).

## 10. Documentation plan

| Audience | Knows | Needs | Pages |
|---|---|---|---|
| Workload users | git, gh, their pipeline | How to get GitHub access in a pipeline or workspace, and why a request was denied | `docs/user/` |
| Admins | Kubernetes, Helm, GitHub Apps | Install, create Apps and installations, write ceilings and policies, read status, audit and metrics | `docs/admin/` |
| Developers | Go, controller-runtime | Architecture, resolution, the fake GitHub, how to run tests | `docs/dev/` |
| Agents | the repo | Playbooks | `docs/agents/` |

Reference tables (CRD fields, flags, metrics) are generated from code.

## 11. Open questions

- Which GitHub API gives Verified commits with full file modes (phase 2 design, live-tested).
- Whether proxy mode should default to allowing every operation the permissions allow (with `deny…` lists) or to an allow-list (phase 2 design).
