# Phase 1 design: token mode

Implements SPEC.md §4 (custom resources, authentication, token-mode resolution, client, audit, metrics, admission) for `docs/acceptance/phase-1.md`. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed.

## Libraries

Never hand-write these (CLAUDE.md, "never roll your own cryptography"):

| Need | Library |
|---|---|
| Controllers, caches, webhook, leader election | `sigs.k8s.io/controller-runtime` (kubebuilder layout) |
| App JWT (RS256) and its transport | `github.com/bradleyfalzon/ghinstallation/v2` `AppsTransport` (uses `golang-jwt/jwt/v5`) |
| GitHub REST calls | `github.com/google/go-github/v<latest>` |
| Concurrent mint de-duplication | `golang.org/x/sync/singleflight` |
| Bounded caches | `github.com/hashicorp/golang-lru/v2/expirable` |
| Git smart HTTP in the fake GitHub | `git http-backend` run through `net/http/cgi` |
| Logging, metrics | `log/slog` (JSON), `prometheus/client_golang` |

## Packages

```
api/v1alpha1                 the three kinds, CEL rules, generated deepcopy/CRDs
internal/permissions         generated GitHub permission catalogue, levels, containment
internal/patterns            repository glob matching and containment
internal/model               immutable snapshot built from the caches (apps, installations, policies, accepted rules)
internal/github              App clients per GitHubApp, installation lookup, token minting, rate-limit probe
internal/auth                Authenticator interface; TokenReview implementation with cache
internal/resolve             token-mode resolution: (identity, request, snapshot) -> decision. Pure.
internal/server              HTTPS token API, health/metrics server, audit
internal/controller          status reconcilers (leader-elected)
internal/webhook             validating webhook
internal/client              client library + subcommands for cmd/githubgate-client
test/fakegithub              fake GitHub (in-process and as a container for e2e)
test/e2e, test/live
```

`resolve` and `permissions`/`patterns` are pure and carry the exhaustive table tests.

## Custom resources

As SPEC.md §4. Details decided here:

- `GitHubApp.spec`: `appID` (int64) xor `appIDSecretRef` (`{name, key}`, key default `app-id`, decimal string, whitespace trimmed); `privateKeySecretRef` (`{name, key}`, key default `private-key.pem`, PEM PKCS#1 or PKCS#8 RSA); `allowedNamespaces`. The xor is a CEL rule.
- `GitHubInstallation.spec`: `appRef`, `owner` (GitHub login; compared case-insensitively), `ceiling` (`[]{repositories []string, permissions map[string]Level}`, min 1), `allowedNamespaces`.
- `GitHubAccessPolicy.spec`: `serviceAccounts` (min 1, DNS-1123 names), `rules` (`listType=map`, key `name`), each: `name`, `installationRef`, `repositories` (min 1), `permissions` (min 1 entry), `token` (`{enabled bool}`, required in phase 1). CEL: at least one mode enabled. Phase 2 adds `proxy`.
- `Level` is an enum `read|write|admin`. Permission names are validated by the webhook against the catalogue (not by CRD enum, so a catalogue update needs no CRD change), and by the model (unknown name → rule not accepted, `InvalidSpec`). Keys are compared exactly (lower-case, as in the catalogue).
- **Phase 1 accepts only repository permissions.** Organization and account permissions (`members`, `organization_secrets`, …) apply to the whole owner whatever `repositories` says, so they can't be limited per repository. The webhook rejects them in rules and ceilings; the model treats them as `InvalidSpec`.
- An installation whose ceiling has an unknown or non-repository permission, or a level the permission doesn't allow, is `Ready=False, reason=InvalidSpec` and grants nothing.
- Several `GitHubInstallation` objects may name the same App and owner, each with its own ceiling and `allowedNamespaces` (this is how different namespaces get different ceilings on one GitHub installation).
- Repository patterns: `^[A-Za-z0-9._*-]{1,100}$`, compared lower-cased. `*` matches any run of characters (names have no `/`).

## Permission catalogue

`make generate` runs `scripts/gen-permissions` against GitHub's REST OpenAPI description (`github/rest-api-description`, file `descriptions/api.github.com/api.github.com.json`, schema `components.schemas.app-permissions`) at a commit pinned in the script (Renovate tracks it). It writes `internal/permissions/catalogue_gen.go`: each permission name with its allowed levels and its scope (`repository`, `organization`, `account`). The scope comes from a short list in the script; generation fails on any name the list doesn't place. `check-generate` keeps it current. Levels order `read < write < admin`; a permission that only has `read`/`write` rejects `admin`.

## Containment (ceiling and rules)

A rule is contained in a ceiling when for every (pattern p, permission n, level l) in the rule there is one ceiling entry with a pattern c that **contains** p and a level for n that is ≥ l.

Pattern containment `contains(c, p)`: true if `c == "*"`, or `p` has no `*` and matches `c`, or `p == c`. Otherwise false ("when in doubt, not contained"). Tested exhaustively, including case and `*` in the middle.

## Model

On any change to a watched object or Secret, a builder produces a new immutable `Snapshot` and swaps it atomically (`atomic.Pointer`). Requests read one snapshot, so one decision never mixes two versions. The snapshot holds, per installation: ready state, installation id, App client handle; per namespace: the accepted rules indexed by ServiceAccount. Not ready until the first full build after cache sync.

Watched: the three kinds, Secrets in the admin namespaces only (`cache.Options.ByObject` with namespace restriction; no cluster-wide Secret access), and Namespaces (for `allowedNamespaces.selector`). Any change to any of them rebuilds the snapshot, so removing a namespace label refuses its next request within seconds.

The snapshot never reads status. Each replica looks up installation ids itself (see "GitHub client"). An installation can serve when its App's key Secret parses and its App ID is set (else `503 unavailable`) and its installation id is known (lookup failure → `503 github_unavailable`).

## GitHub client

- One `AppsTransport` per `GitHubApp`, rebuilt when the key or App ID changes (keyed by the Secrets' `resourceVersion`s).
- Installation id: every replica looks it up itself: `GET /app/installations` (paginated), match `account.login` case-insensitively against `owner`. Cached per (App ID, lower-cased owner) for 10 minutes, dropped when `spec.owner` or `appRef` changes, and refreshed on a `404` from the token endpoint. Refreshes are de-duplicated per App (singleflight) and at most one every 30 s. Not found → `InstallationNotFound`.
- App check: `GET /app` for the slug and status. Installation status also records the installation's granted permissions; a ceiling permission above them sets condition `CeilingExceedsGrant=True` (informational; the ceiling still caps, GitHub refuses the excess with `422`).
- Mint: `POST /app/installations/{id}/access_tokens`. **GitHub gives all repositories when `repositories` is omitted, and the installation's full grant when `permissions` is omitted**, so:
  - the body is built as `{"repositories": [...], "permissions": map[string]string}` through go-github's `client.NewRequest`/`Do`, never through `InstallationTokenOptions`/`InstallationPermissions` (whose `omitempty` fields and fixed struct drop empty or unknown values);
  - the mint function refuses, without calling GitHub, if either set is empty;
  - the response is checked: `repositories` must be exactly the requested names (case-insensitive), and `permissions` at most the requested ones plus `metadata: read`. On any mismatch the token is discarded and the caller gets `503 github_unavailable`.
  GitHub caps `repositories` at 500; more → `400` to the caller.
- GitHub's answers:

  | GitHub answer | Caller gets | Status written | Retry/refresh |
  |---|---|---|---|
  | `422` (repository not accessible, permission not granted) | `403 github_refused`, GitHub's message | — | no |
  | `401` on the JWT | `503 github_unavailable` | App `Ready=False, SecretInvalid` | no |
  | `403` installation suspended | `403 github_refused` | installation `Ready=False, InstallationSuspended` | no |
  | `403`/`429` rate limit | `503 github_unavailable`, `Retry-After` passed on | — | no |
  | `404` from the token endpoint | after one id refresh, retried once; still `404` → `503 github_unavailable` | installation `InstallationNotFound` | refresh as above |
  | `5xx`, network error, timeout | `503 github_unavailable` | — | no |
- Rate limit: after each mint, one `GET /rate_limit` with the new token (doesn't count against the limit) updates `githubgate_github_rate_limit_remaining{installation}`; at most once a minute per installation.
- Timeouts: 10 s per GitHub call.

## Authentication

`type Authenticator interface { Authenticate(ctx, r *http.Request) (Identity, error) }`. Errors distinguish *no credential*, *invalid credential* and *unavailable*.

The TokenReview implementation needs exactly one `Authorization` header with scheme `Bearer`, `token` (case-insensitive) or `Basic` (the token is the password). More than one header, another scheme or an empty value → `401` with `WWW-Authenticate: Bearer`. Audience from `--audience` (default `githubgate`), must be in `status.audiences`. Cache: `expirable.LRU`, 10 000 entries, 30 s, key SHA-256(token), definite results only (authenticated / not authenticated). API errors → `503`, never cached. Identity requires username `system:serviceaccount:<ns>:<name>`; anything else is invalid.

## Token API

Served on `:8443` (TLS; certificate and key files from `--tls-cert-file` / `--tls-key-file`, reloaded on change via `certwatcher`). Health and metrics on `:8080`.

`POST /v1/token`, JSON body (all optional):

```json
{"installation": "githubgate/soliddowant", "owner": "solidDoWant", "repositories": ["foo"], "permissions": {"contents": "read"}}
```

Installation choice: `installation` (`ns/name`) if given (with an `owner` that doesn't match its `spec.owner` → `400 invalid_request`). Otherwise the candidates are the installations (of `owner`, if given) whose candidate rules cover **every requested repository and every requested permission**. One candidate → use it; none → `403 denied`; more than one → `409 ambiguous_installation`. So a caller with App1's rule for `foo` and App2's rule for `bar` on one owner can still `git clone foo`.

Request validation (all `400 invalid_request`): unknown JSON fields (`DisallowUnknownFields`); a repository that isn't a literal name `^[A-Za-z0-9._-]{1,100}$` or is `.`/`..` (duplicates are removed case-insensitively); a permission not in the catalogue, not a repository permission, or a level it doesn't allow; no repositories named when the caller's rules for the installation are only patterns ("name the repositories: your rules only use patterns").

Permissions with none requested: within one repository, levels from several rules combine as the **highest**; across the requested repositories each permission gets the **lowest** level every repository has (`foo: contents write`, `bar: contents read` → `contents: read`). **If the result is empty, the request is refused with `403 denied`** and a message naming the repositories and saying they share no permission. The same applies to an empty repository set.

Response `200` (the `repositories` and `permissions` are GitHub's values, so `metadata: read` appears):

```json
{"token": "ghs_…", "expiresAt": "…", "repositories": ["foo"], "permissions": {"contents": "read"}}
```

Errors: JSON `{"error": "<reason>", "message": "<human text>"}` with `400 invalid_request`, `401 unauthenticated`, `403 denied|github_refused`, `409 ambiguous_installation`, `503 unavailable|github_unavailable|not_ready`. Messages name namespace, ServiceAccount, repository and permission, never a token. Body limit 64 KiB.

Resolution is SPEC.md §4 "Resolution (token mode)", implemented in `internal/resolve` as a pure function returning the decision, the chosen installation, the final repositories and permissions, and the matching rules (for audit).

Token cache: `expirable.LRU`, 10 000 entries, key (caller namespace/ServiceAccount, **App ID, GitHub installation id**, sorted repositories, sorted permissions) — the values this request resolved to, never the object UID, so an `owner` or `appRef` change can't hand out a token from the old installation. A cached token is handed out only while it has ≥ 10 minutes left; mints for the same key are de-duplicated with singleflight, run with a detached context (`context.WithoutCancel` plus the 10 s timeout) so one caller's disconnect doesn't fail the others. The cache is per replica and in memory only.

Shutdown: on `SIGTERM`, `/readyz` fails at once, the server keeps serving for `--shutdown-delay` (default 5 s), then `http.Server.Shutdown` with a 20 s deadline. The chart sets `terminationGracePeriodSeconds: 30`. Checklist 25's "kill" means a pod delete.

TLS: the server refuses to start without readable TLS files. A snapshot change that removes the matching rule invalidates nothing already handed out (SPEC) but the next request re-resolves before using the cache, so a removed grant is refused at once.

## Status controller

Leader-elected, one reconciler per kind; writes `observedGeneration`, `Ready`, and the policy's `rules[]` with `Accepted` using the same model code that serving uses (one function decides acceptance). Re-queues installations every 10 minutes for the GitHub checks. Mappings: an App change enqueues its installations; an installation change enqueues the policies that reference it (field index on `rules[].installationRef`); a Namespace change enqueues the policies in it and the installations/Apps whose selectors may match it.

Rule `Accepted` reasons, first match wins:

| Order | Reason | When |
|---|---|---|
| 1 | `InvalidSpec` | unknown or non-repository permission, or a level it doesn't allow |
| 2 | `RefNotFound` | the installation doesn't exist |
| 3 | `NotAllowed` | the installation doesn't admit the policy's namespace (checklist 14) |
| 4 | `OutsideCeiling` | the rule isn't contained in the ceiling (checklist 13) |

A rule whose installation exists but isn't ready is **accepted** (acceptance is static), with `Accepted=True, reason=RefNotReady` as a note; requests through it get `503`. Policy `Ready`: `True` if every rule is accepted, else `False, reason=RulesNotAccepted` while the accepted rules still apply. `OutsideCeiling` is a rule-only reason. `GitHubInstallation`: `RefNotFound`/`NotAllowed` for its `appRef` (the App's `allowedNamespaces`), `InvalidSpec` for its ceiling, `InstallationNotFound`, `InstallationSuspended`, `GitHubUnavailable`.

## Admission webhook

Validating, `--webhook` on, cert from files (cert-manager in the chart). Rejects: App/Installation outside admin namespaces; unknown permission name or level; a rule outside its installation's ceiling when the installation exists (references to missing objects are allowed). Failure policy `Fail` for these three kinds only.

## Client (`githubgate-client`)

Config by env or flags: `GITHUBGATE_URL` (default `https://githubgate.githubgate.svc:8443`; an `http://` URL is refused, since it would send the ServiceAccount token in clear text), `GITHUBGATE_CA_FILE` (default `/var/run/secrets/githubgate/ca.crt`, trusted in addition to the system roots), `GITHUBGATE_TOKEN_FILE` (default `/var/run/secrets/githubgate/token`), read on every call.

`POST /v1/token` is retried once, after 200–500 ms, on a connection error or `503 not_ready` (safe: a mint has no side effect the caller can see).

- `token`: flags as SPEC; prints the token only, to stdout.
- `git-credential get`: reads git's key=value input; acts only for `protocol=https` and `host=github.com`; with `path=owner/repo(.git)` asks for that repository (owner from the path), without a path asks with only the owner unset (default resolution). Prints `username=x-access-token`, `password=<token>`, `password_expiry_utc=<unix>` (git 2.41+ honours it; the client never relies on that, and its in-process cache checks expiry itself). **On refusal it prints `quit=1`** and githubgate's message to stderr, so git stops without asking later helpers or `GIT_ASKPASS` (Coder workspaces set `GIT_ASKPASS` to their broad token). Git then fails with its "credential helper told us to quit" error; with githubgate's reason on stderr, that is checklist 21's "normal authentication error". `store`/`erase`: no-op.
- `configure`: runs `git config --global` for `credential.https://github.com.helper` = `!githubgate-client git-credential` (replacing other helpers for that URL with an empty entry first) and `credential.https://github.com.useHttpPath=true`. It warns when `GIT_ASKPASS` or a repository-local helper is set.
- `gh [--repo owner/name] -- <gh args>`: gets a token (for `--repo`, else from the current directory's `origin` remote on github.com, else default resolution) and execs `gh` with `GH_TOKEN` set. Every invocation gets a fresh (or cached-by-server) token, so long sessions never use an expired one.

## Audit and metrics

Audit: one `slog` JSON line per `/v1/token` request, fields as SPEC.md §4, plus `reason` and `request_id`. Metrics: `githubgate_requests_total{mode,decision,code_class}`, `githubgate_github_request_duration_seconds{operation}`, `githubgate_token_cache_total{result}`, `githubgate_tokenreview_total{result}`, `githubgate_github_rate_limit_remaining{installation}`.

## Fake GitHub

`test/fakegithub`, usable in-process (`httptest`) and as an e2e container image (`ghcr.io/soliddowant/githubgate-fakegithub`, built in CI, never released). It copies GitHub's real behaviour for:
- App JWT: RS256 only, `iss` = App ID, `iat` ≤ now + 60 s, `exp` > now − 60 s, `exp` ≤ now + 10 min + 60 s, else `401`. (GitHub's own recipe, `iat = now − 60 s` and `exp = now + 600 s`, must pass.) Every JWT received is recorded for the leak search.
- Every installation token includes `metadata: read`, as on GitHub. Checklist 1 compares with it added.
- Repositories are private by default. Git over HTTPS answers like github.com: no credential → `401` with `WWW-Authenticate: Basic realm="GitHub"`; a token without access to the repository → `404` "Repository not found"; a push without `contents: write` → `403`.
- `GET /app`, `GET /app/installations` (paginated, `per_page` respected), `POST /app/installations/{id}/access_tokens` (repositories by name, 500 max, `422` with GitHub's messages for an inaccessible repository or a permission above the installation's grant; no `permissions` → the installation's full grant; expiry one hour).
- Installation-token requests: `GET /rate_limit`, `GET /repos/{o}/{r}`, `POST /repos/{o}/{r}/releases` (needs `contents: write`), issue comments (`issues: write`); `403` "Resource not accessible by integration" like GitHub when the token lacks the permission or repository.
- Git smart HTTP for a set of bare repositories via `git http-backend`, with Basic auth (`x-access-token` / token) enforcing the token's repositories and `contents` level (fetch: read; push: write).
- A test control API (separate port) to create apps, installations, repositories, to suspend an installation, and to read the mint log (scenarios 2, 12) and every token and JWT issued or received (leak search, scenario 26).

Each quirk copied from GitHub gets a comment with its documentation link; the live test re-checks the ones it can.

## e2e

Scenario 12 ("one mint") is tested against one replica in an integration test, because the cache is per replica. Scenario 7 (clone yes, release no) is tested in e2e against the fake's git and releases endpoint. The leak search (26) covers every token issued, every JWT the fake received, every ServiceAccount token the test pods used, and the PEM body of each test key.

kind cluster, the chart with 3 replicas, the fake GitHub in-cluster, a ServiceAccount-per-scenario test namespace, real `git` in a test pod. Scenario 22 runs in CI with a stub `gh` that records `GH_TOKEN` per call (and the fake's mint log proves a fresh token after expiry is simulated by the fake's clock control); the live test runs real `gh`.

## Live test

`make test-live` (`//go:build live`), run by hand by the operator. Inputs: `GITHUBGATE_LIVE_APP_ID`, `GITHUBGATE_LIVE_KEY_FILE`, `GITHUBGATE_LIVE_OWNER`, `GITHUBGATE_LIVE_REPO` (a throwaway repository the test App is installed on). Runs the server in-process with envtest, covers scenarios 1, 4, 7, 20 and real `gh issue comment` (22), deletes any release and tag it creates, and finally searches its own output for every token it saw.

## Chart

`deploy/charts/githubgate`: a cert-manager Certificate for the token API (`githubgate.<ns>.svc`, `.svc.cluster.local`) with its CA published in a ConfigMap `githubgate-ca` for clients to mount at `/var/run/secrets/githubgate/ca.crt`; Deployment (3 replicas, `terminationGracePeriodSeconds: 30`, PDB `maxUnavailable: 1`, memory request = limit, no CPU limit), Service (8443, 8080), RBAC as SPEC.md §5 (Secrets via Role in each admin namespace), CRDs, optional webhook with cert-manager Certificate, optional ServiceMonitor, `--admin-namespaces` default release namespace. Images by digest.

## Issue order

0. `cmd/githubgate` wiring (manager, flags, leader election, both listeners, TLS, shutdown) once the server pieces exist
1. API types + permission catalogue + patterns (parallel)
2. fake GitHub (parallel with 1)
3. GitHub client, auth (parallel, need 1/2)
4. model + resolve (need 1)
5. token API server, status controller, webhook (need 3/4)
6. client (needs the API's request/response types only, so it starts after 1)
7. chart, e2e, docs, live test, release
