# Phase 2 design: proxy mode and signed commits

Implements SPEC.md §4 "Proxy mode" and "Signed commits" for `docs/acceptance/phase-2.md`. Builds on `docs/design/phase-1.md`; everything there still holds unless this file says otherwise. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed. Revised after the design review on #77.

## Decisions that answer SPEC.md §11

- **Which API signs commits** (checked by the operator on github.com with the test App, 2026-11-20):
  - Git Database REST API commit with a custom `author`: **not** signed.
  - Git Database REST API commit with no `author`/`committer`: **Verified**; author is `<app>[bot]`, committer `GitHub`; file modes `100755` and symlinks (`120000`) are kept.
  - GraphQL `createCommitOnBranch`: Verified, but it can't set modes or symlinks.

  So `push` uses the Git Database REST API with no author or committer, and keeps the original author as a `Co-authored-by:` trailer.
- **Default for operations:** a proxy rule allows every REST operation and GraphQL mutation its permissions allow (GitHub enforces the permissions), minus `denyOperations` / `denyMutations`. If `allowOperations` / `allowMutations` is set, only those are allowed (deny still wins). Requests the proxy can't classify are always refused.

## Libraries

| Need | Library |
|---|---|
| REST operation catalogue | generated from the same pinned `github/rest-api-description` file as the permission catalogue |
| GraphQL mutation catalogue | generated from `octokit/graphql-schema` (`schema.graphql`) at a pinned commit (Renovate tracks it), parsed with `gqlparser`'s schema parser |
| GraphQL documents | `github.com/vektah/gqlparser/v2/parser` (parse) and `.../formatter` (print the classified document) |
| git receive-pack | `github.com/go-git/go-git/v5/plumbing/protocol/packp` (`ReferenceUpdateRequest` decoding of the command section, `ReportStatus` encoding) and `.../sideband` muxer; `pktline` for reading up to the flush |
| Reverse proxy | `net/http/httputil.ReverseProxy` with `Rewrite` (never `Director`) |
| TLS by server name | `crypto/tls` `GetCertificate` with certificates from files, reloaded by `certwatcher` |
| Commit trailers | `git interpret-trailers` (run by the client) |
| Glob matching for refs and operation ids | `internal/patterns`, extended as in "Patterns" |

## Custom resource changes

`AccessRule` gains `proxy` (`*ProxyMode`):

```yaml
proxy:
  enabled: true
  pushRefs:            # optional; when set, every ref the request writes must match one
    - refs/heads/agent/**
  denyOperations:      # optional REST operation ids
    - repos/create-release
  denyMutations:       # optional GraphQL mutation names
    - mergePullRequest
  # allowOperations / allowMutations: optional; when set, only these are allowed
```

- CEL: at least one of `token.enabled`, `proxy.enabled`; if `token.enabled` is true, `proxy.pushRefs` and the four lists must be unset (a token in the caller's hands can't enforce them). The existing CEL rule requiring `token.enabled` becomes "token or proxy enabled".
- `pushRefs` and the four lists have `MinItems=1`: an empty list is invalid, so "unset" and "empty" can't be confused.
- **Patterns:** operation ids `^[a-z0-9*/-]+$`; mutation names `^[A-Za-z0-9*]+$`; `*` matches one or more characters within one segment. Refs: must start with `refs/`, characters as `git check-ref-format` allows (no `..`, `@{`, `\`, control characters); `*` matches one or more characters within one segment, `**` one or more whole segments (`refs/heads/agent/**` does not match `refs/heads/agent`).
- **Every list entry must match the current catalogue.** The webhook rejects an entry that matches nothing, and the model (the acceptance function both the controller and serving use) marks a rule `Accepted=False, reason=InvalidSpec` naming the entry when a catalogue update leaves an entry matching nothing. Such a rule grants nothing, so a renamed operation never silently becomes allowed.

Model: rules carry their modes. Token mode uses rules with `token.enabled`; the proxy uses rules with `proxy.enabled`. Containment against the ceiling is unchanged.

## Catalogues

`scripts/gen-permissions` also writes:

- `internal/operations/rest_gen.go`: for every path and method in the REST description, the operation id, the path template, and its parameters. Parameters that may contain `/` are listed explicitly in the generator (`ref`, `path`, `basehead`, `branch`, and any others a test finds: the generator fails on a template whose parameter name is in a "may contain slash" review list but not classified). Such a parameter matches one or more segments greedily; a template may have at most one, followed only by literals. Matching: method and segments against the templates, literal segments before parameters, the most literal template wins; no match → unknown. `HEAD` is classified as the `GET` of the same path.
- `internal/operations/mutations_gen.go`: the field names of the `Mutation` type from the pinned GraphQL schema.

## Ref-writing operations

When the rule that allows a request has `pushRefs`, every ref the request writes must match it. `internal/operations/refwrites.go` holds a reviewed table:

| Operation | Ref(s) written | Check |
|---|---|---|
| `git/create-ref` | body `ref` | must match `pushRefs` |
| `git/update-ref`, `git/delete-ref` | `refs/` + path `{ref}` | must match `pushRefs` |
| `repos/create-release`, `repos/update-release` | `refs/tags/` + body `tag_name` (creates the tag if missing) | must match `pushRefs`. On update without `tag_name`: refused if the body sets `draft` to `false` (publishing a draft creates its tag, which the proxy can't see without a lookup); otherwise it writes nothing |
| `repos/create-or-update-file-contents`, `repos/delete-file` | `refs/heads/` + body `branch` (default branch if absent) | refused (the default branch can't be known without a lookup) |
| `repos/merge`, `repos/rename-branch`, `repos/merge-upstream`, `pulls/merge`, `pulls/update-branch` | a branch | refused |
| GraphQL `createRef`, `updateRef`, `updateRefs`, `deleteRef`, `createCommitOnBranch`, `mergeBranch`, `mergePullRequest`, `enablePullRequestAutoMerge`, `updatePullRequestBranch`, `revertPullRequest`, and every other mutation the table marks as writing refs | refs by node id | refused |
| git `receive-pack` | every command's ref | must match `pushRefs` |

Bodies that must be read for this table are read whole (limit 1 MiB) and must be valid JSON with no duplicate keys (same decoder as GraphQL); the proxy forwards the bytes it read. **Completeness test:** every REST operation and mutation whose id or name matches `(?i)ref|commit|merge|branch|tag|release|content|file` must appear in the table as either ref-writing (with its check) or reviewed as not writing refs. A catalogue update that adds such an entry fails the test until someone classifies it. Unclassified entries are refused under `pushRefs` meanwhile (fail closed).

## Proxy

### Serving

A second TLS listener on `:9443` (Service `githubgate-proxy`, port 443 → 9443), on only with `--proxy`. The chart takes a `proxy.ca` Issuer reference (the admin's name-constrained CA; an X.509 name constraint `github.com` also permits its subdomains, so the admin doc says "permitted subtree github.com", and scenario 15 is enforced by `GetCertificate`) and issues one certificate with SANs `api.github.com`, `uploads.github.com`, `github.com`. A ClientHello for any other name, or none, fails the handshake. The chart can pin the Service's `clusterIP` (`proxy.clusterIP`) so `hostAliases` can name it.

**Host.** SNI only picks the certificate; the `Host` header decides. `Host` is lower-cased, may carry `:443`, must not have a trailing dot, and must be exactly one of the three hosts; anything else → `421 Misdirected Request`, never forwarded. **The upstream scheme and host come from a fixed table keyed by the classified host, never from the request.**

**Upstreams** are flags with chart values: `--proxy-upstream-api` (default `https://api.github.com`), `--proxy-upstream-uploads` (`https://uploads.github.com`), `--proxy-upstream-git` (`https://github.com`), `--proxy-upstream-ca-file` (extra CA, for the fake in e2e).

**Timeouts.** Listener: `ReadHeaderTimeout` 10 s, no `ReadTimeout`/`WriteTimeout`; an idle-progress timeout of 5 minutes with no bytes in either direction. Upstream transport: dial 10 s, response header 60 s. Unreachable upstream or timeout → `503 github_unavailable` (the same meaning as in the token API).

### Authentication

As phase 1: one `Authorization` header (`Bearer`, `token`, or Basic password = ServiceAccount token). git sends Basic; `gh` sends `token`. No credential → `401` with `WWW-Authenticate: Basic realm="GitHub"` (so git asks its credential helper). The client's credential is always removed before forwarding.

### Classifying a request

Before anything is forwarded:

1. **Refused outright** (`400`): methods `CONNECT`, `TRACE`, `OPTIONS`; any `Upgrade` header; method-override headers (`X-HTTP-Method-Override`, `X-HTTP-Method`, `X-Method-Override`) or a `_method` query or form field. A request `Content-Encoding` other than `identity` → `415`, with one exception: `gzip` is accepted on `git-upload-pack` (git compresses that body when it is over 1 KiB) and forwarded unchanged, because the proxy never reads that body to decide. Bodies the proxy reads to decide (GraphQL, the ref-writing REST operations, `git-receive-pack`) still get `415` for any encoding.
2. **Path hygiene** (`400`): `.`/`..` segments, empty segments, `%2F`, `%5C`, percent-encoding of unreserved characters. Every query is parsed with `url.ParseQuery` and re-encoded; an unparseable query → `400`. The forwarded URL is rebuilt from the parsed parts.
3. **Host `api.github.com`:** `POST /graphql` → GraphQL (below); anything else → REST: match the catalogue; unknown → `403 unknown_operation`.
4. **Host `uploads.github.com`:** REST against the same catalogue.
5. **Host `github.com`:** only git smart HTTP for `/<owner>/<repo>(.git)/info/refs?service=git-upload-pack|git-receive-pack`, `…/git-upload-pack`, `…/git-receive-pack`. Anything else → `404`, except a `GET` or `HEAD` web path that a rule's `webPaths` allows (`docs/design/phase-3.md`).

Result: the owner and repository (if any), the operation id or mutation names, the refs written, and the level the request needs (git fetch: `contents: read`; push: `contents: write`; REST/GraphQL: left to GitHub).

### Deciding

The candidate rules are the caller's accepted rules with `proxy.enabled`.

A candidate rule **allows** a request when:
- it covers the request's repository (or, for a request without a repository, it has exact-name repositories);
- the request passes its operation or mutation lists (for GraphQL, every mutation in the document);
- every ref the request writes matches its `pushRefs` (if set), and the request isn't a refused ref-writing operation under it;
- for git, it gives the needed `contents` level.

**Only allowing rules count.** The installation and the token come from the allowing rules alone, never from other rules that merely cover the repository, so one rule's lists or `pushRefs` can't be undone by another rule's permissions.

- **With a repository** (REST with `{owner}/{repo}`, git): the allowing rules for that repository. Their installations: exactly one → use it; more than one → `409 ambiguous_installation`; none → `403 denied` naming the repository and why (not covered, operation denied, ref outside `pushRefs`).
- **Without a repository** (GraphQL; REST without `{repo}`): REST without `{repo}` is allowed only if listed in `allowOperations` of an allowing rule. The allowing rules' installations must be exactly one (`409` otherwise). If no allowing rule has an exact-name repository → `403 denied` "your proxy rules only use patterns; GraphQL needs exact repository names". GraphQL queries are allowed by every candidate rule; mutations only by rules whose lists allow all of them and that aren't stopped by the ref-writing table.
- **Token:** repositories = the request's repository, or without one the exact-name repositories of the allowing rules; permissions = the union (highest level) of the allowing rules for one repository, or for several the lowest level each repository has across the allowing rules (phase 1 rule). Same cache as phase 1.

Decision table (each row a test):

| Rules | Request | Result |
|---|---|---|
| A: `foo` contents write, deny `repos/create-release`; B: `foo` issues write | create release on `foo` | B allows it but gives no `contents`; token = issues write → GitHub refuses. A denies. Net: not created |
| A: `foo` contents write, `pushRefs: [refs/heads/agent/**]`; B: `foo` contents read | push `refs/heads/master` | A refuses the ref; B lacks `contents: write` → `403` |
| A: `foo` pull_requests write, deny `mergePullRequest`; B: `bar` pull_requests write | `mergePullRequest` (any PR) | only B allows; token covers `bar` only → a `foo` PR can't be merged |
| A: `foo` contents write, `pushRefs: [refs/heads/agent/**]` | `PATCH git/refs/heads/master` | `403` ref outside `pushRefs` |
| A: `foo` contents write, `pushRefs` set | GraphQL `updateRefs` | `403` ref-writing mutation under `pushRefs` |
| App1 rule `foo`, App2 rule `bar`, same owner | `git clone foo` | App1 only → allowed |

### Forwarding

`httputil.ReverseProxy` with `Rewrite`: URL from the upstream table, path and query from the parsed request; credential for `api`/`uploads`: `Authorization: token <installation token>`; for `github.com` (git): `Authorization: Basic base64("x-access-token:" + token)`. `Host` set to the upstream's host; hop-by-hop headers, `Cookie` and any `X-Forwarded-*` dropped. Bodies streamed (except the bodies read whole above, which are replayed). Responses passed through except `Set-Cookie`; redirects to other hosts (archives, release assets on GitHub's storage) are passed through unchanged, as their URLs carry no installation token.

### Errors

| Host | Body |
|---|---|
| API hosts (REST) | `Content-Type: application/json`, `{"message": "githubgate: <reason>", "error": "<code>", "documentation_url": "<link to docs/user page>"}` |
| GraphQL | same status and fields, plus `"errors": [{"message": "githubgate: <reason>"}]` |
| `github.com` | `text/plain` reason (receive-pack refusals as below) |

Codes: `400 invalid_request`, `401 unauthenticated`, `403 denied|unknown_operation|github_refused`, `404`, `409 ambiguous_installation`, `413 too_large` (GraphQL or ref-table body over 1 MiB, receive-pack command section over its limits), `415`, `421`, `503 unavailable|not_ready|github_unavailable` (TokenReview down, caches not synced, minting failures as in phase 1's GitHub-answers table, upstream unreachable). Reasons name the repository, operation or mutation, or ref.

### GraphQL

Body limit 1 MiB. The top level is decoded with a `json.Decoder` token pass: it must be an object (a JSON array, a batch, is refused), keys are compared **exactly** (no case folding), duplicate keys are refused, and only `query`, `operationName`, `variables`, `extensions` are allowed. Persisted-query `extensions` without `query` are refused. The document must parse; several operations need an `operationName` that names one; subscriptions are refused. Mutation names are the top-level field names of the selected operation (aliases ignored); a fragment spread or inline fragment at the top level of a mutation is refused.

**The forwarded body is rebuilt by githubgate** with `encoding/json` from the classified values, with `query` printed by `gqlparser`'s formatter from the parsed document. GitHub runs exactly the document that was classified.

### git receive-pack

Read pkt-lines up to the flush packet: optional `shallow <oid>` lines, then commands `<old> <new> <ref>` (the first with capabilities after NUL). Anything else (`push-cert`, malformed oids, refs that fail `check-ref-format`) → refused. Limits: 1000 commands, 64 KiB command section → `413`. Every ref must match `pushRefs` of an allowing rule (see "Deciding").

A refusal is answered so git shows it:
- the rest of the request body is read and discarded first (bounded at 100 MiB or the idle timeout), so git doesn't see a broken pipe;
- `200`, `Content-Type: application/x-git-receive-pack-result`;
- the report (`report-status` or `report-status-v2`, as the client asked) is `unpack ok` and an `ng <ref> githubgate: <reason>` for every ref: refused ones with their reason, allowed ones with `githubgate: push refused (another ref)`; nothing is forwarded;
- if the client asked for `side-band-64k`/`side-band`, the report goes on channel 1 and a `githubgate: <reason>` line on channel 2, using go-git's sideband muxer and `packp.ReportStatus` encoder;
- if the client asked for no report, `403` with the reason as text.

### Audit and metrics

One audit line per proxied request, written when the response finishes or the client disconnects: `mode=proxy`, namespace, ServiceAccount, pod, `host`, `method`, `operation` or `mutations[]`, `refs[]`, `repositories`, `permissions`, installation, `decision`, `reason`, `github_status`, `bytes`, `duration`. The path and query are not logged (they may hold user data). `githubgate_requests_total` gets `mode="proxy"`.

### Interaction table

Every proxied request runs against one snapshot and one token. What happens when state changes while it is in flight:

| Change during a request | Effect |
|---|---|
| Policy removes the rule | the request already decided continues with its token; the next request is refused |
| Token near expiry in cache | cache hands out tokens with ≥ 10 min left; long git pushes keep the token they started with (GitHub checks it at the start) |
| Replica receives SIGTERM | shutdown sequence from phase 1; in-flight streams get the 20 s deadline |
| Installation id refreshed | the token in hand stays valid; new requests use the new id |
| CA / certificate rotated | new handshakes use the new certificate; open connections keep the old one |
| GitHub returns `401` for the token | not retried; `503 github_unavailable` |
| Catalogue update leaves a list entry matching nothing | the rule becomes `InvalidSpec` and grants nothing from the next snapshot |

## Client changes

- `configure --proxy`: sets `credential.https://github.com.helper` to `!githubgate-client git-credential --proxy` (answers `username=x-access-token` and the ServiceAccount token, read fresh; never calls the token API), and `http.https://github.com/.sslCAInfo` to the proxy CA file **only** (not added to the system bundle). A pod or VM that reaches the real github.com by mistake then fails TLS instead of sending the ServiceAccount token to GitHub.
- `gh --proxy -- …`: runs `gh` with `GH_TOKEN` set to the ServiceAccount token and `SSL_CERT_FILE` set to a bundle of the proxy CA **and** the system CAs, so `gh` can follow GitHub's redirects to its storage hosts (`codeload.github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com`) for downloads. Because the system CAs would also accept the real GitHub, `gh --proxy` first resolves `api.github.com`, `uploads.github.com` and `github.com` and refuses to run (naming the host) unless every address of each is the proxy's address (`GITHUBGATE_PROXY_ADDRESS`, set by the user guide's pod setup, or the addresses of the proxy Service's name `GITHUBGATE_PROXY_HOST`). A pod without the `hostAliases` then fails before any token is sent.

## Signed commits: `githubgate-client push [--proxy] [<remote> [<branch>]]`

Default remote `origin`, branch the one `HEAD` points to (`git symbolic-ref HEAD`; a detached `HEAD` is refused). Token mode asks the token API for `contents: write` on the repository; `--proxy` sends the requests through the proxy, where `git/update-ref`/`git/create-ref` are checked against `pushRefs`.

1. `git fetch <remote> <branch>`; the range is `<remote>/<branch>..HEAD` (if the remote branch doesn't exist: from the merge base with the remote's default branch). Refuse if `HEAD` doesn't contain the remote branch (needs a rebase).
2. Refuse, naming the commit, if any commit in the range has more than one parent, or touches a submodule (`160000`). Nothing is published.
3. For each commit, oldest first: for each added or changed file (content or mode), use the existing blob if the repository has it (`GET /git/blobs/{sha}`, not a content-creating request), else create it (`POST /git/blobs`, base64); create a tree with `base_tree` = the previous published tree and entries for changes (mode `100644`/`100755`/`120000`; deletions as `sha: null`; mode-only changes included); create a commit (`POST /git/commits`) whose message is the original message with a `Co-authored-by: <name> <email>` trailer for the original author added by `git interpret-trailers --if-exists addIfDifferent`, the tree, and the previous published commit as parent. **No `author` or `committer` field.** On `403`/`429` with a rate-limit message, stop, print GitHub's message, publish nothing.
4. Move the branch once at the end: `PATCH /git/refs/heads/<branch>` with `force: false` (or `POST /git/refs` to create). If the remote moved meanwhile, nothing is changed and `push` says so.
5. `git fetch`; if the last published commit's tree equals the local `HEAD` tree, `git update-ref <HEAD's branch> <last published> <old HEAD>` (the index and working tree need no change); otherwise leave the local branch alone and print both tree ids.

Blobs over 100 MB are refused with GitHub's limit named.

## Fake GitHub additions

The REST operations and GraphQL mutations the scenarios use (`createPullRequest`, issue comments, releases, release asset upload on an `uploads` host, archive redirect, `git/refs` create/update), git receive-pack with `report-status`, `report-status-v2` and side-band as GitHub advertises them, git authentication by Basic `x-access-token` only, and the Git Database endpoints (`blobs` incl. `GET`, `trees` with `base_tree`, deletions and modes, `commits` with the signing rule: an author field → unsigned, none → verified with the App as author, `refs`). The fake serves the three host names with a test CA, so e2e pods use real `hostAliases` and githubgate uses the `--proxy-upstream-*` flags.

## Testing

- Unit: classification and decision tables (the decision table above, paths, method overrides, operation matching incl. slash parameters and `HEAD`, GraphQL shapes from scenario 10 plus case-variant and duplicate keys, the ref-writing table and its completeness test, pkt-line and push-cert input), exhaustive.
- Fuzz: the path parser, the GraphQL top-level decoder and the receive-pack command reader (`go test -fuzz`, 30 s each in CI).
- e2e: proxied pods with `hostAliases` to the proxy and the fake behind it.
- Live (operator): scenarios 1, 6, 9, 12, 18 on github.com; 18 checks `verification.verified`.

## Issue order

1. CRD `proxy` fields, webhook, model modes and catalogue-match acceptance; REST and mutation catalogues with slash parameters and the ref-writing table (parallel)
2. fake GitHub additions (parallel)
3. proxy listener, Host/TLS, upstream table, auth, request hygiene, REST and git classification; GraphQL decoding, classification and re-serialisation (parallel, need 1)
4. decision + forwarding + errors + audit (needs 3); receive-pack refs and refusal reports (needs 3)
5. client `--proxy` and `push` (push needs only 2)
6. chart, e2e, docs, live
