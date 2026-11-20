# Phase 2 design: proxy mode and signed commits

Implements SPEC.md §4 "Proxy mode" and "Signed commits" for `docs/acceptance/phase-2.md`. Builds on `docs/design/phase-1.md`; everything there still holds unless this file says otherwise. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed.

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
| GraphQL parsing | `github.com/vektah/gqlparser/v2/parser` (parse only; no schema) |
| git pkt-line | `github.com/go-git/go-git/v5/plumbing/format/pktline` (reading the receive-pack command list only) |
| Reverse proxy | `net/http/httputil.ReverseProxy` with `Rewrite` (never `Director`) |
| TLS by server name | `crypto/tls` `GetCertificate` with certificates from files, reloaded by `certwatcher` |
| Glob matching for refs and operation ids | `internal/patterns` (extended with `**` for refs; see below) |

## Custom resource changes

`AccessRule` gains `proxy` (`*ProxyMode`):

```yaml
proxy:
  enabled: true
  pushRefs:            # optional; when set, every ref a push updates must match one
    - refs/heads/agent/**
  allowOperations: []  # optional REST operation ids, globs with *
  denyOperations:
    - repos/create-release
  allowMutations: []   # optional GraphQL mutation names, globs with *
  denyMutations:
    - mergePullRequest
```

CEL: at least one of `token.enabled`, `proxy.enabled`; if `token.enabled` is true, `pushRefs` and the four lists must be empty (a token in the caller's hands can't enforce them). Patterns: operation ids `^[a-z0-9*/-]+$`; mutations `^[A-Za-z0-9*]+$`; refs must start with `refs/`, `*` matches within one path segment and `**` across segments. The webhook rejects an operation id or mutation pattern that matches nothing in the catalogue (so typos don't silently allow or deny nothing); a pattern with `*` must match at least one entry.

Model: rules carry their modes. Token mode uses rules with `token.enabled`; the proxy uses rules with `proxy.enabled`. A rule with both is in both. Containment against the ceiling is unchanged.

## Operation catalogue

`scripts/gen-permissions` also writes `internal/operations/catalogue_gen.go`: for every path and method in the description, the operation id, the path template, and whether the template has `{owner}` and `{repo}`. Matching a request: method plus path segments against the templates, literal segments before parameters, the most literal template wins; no match → unknown.

## Proxy

### Serving

A second TLS listener on `:9443` (Service `githubgate-proxy`, port 443 → 9443). The chart takes a `proxy.ca` Issuer reference (the admin's name-constrained CA, whose constraints cover exactly the three hosts) and issues one certificate with SANs `api.github.com`, `uploads.github.com`, `github.com`. A ClientHello for any other name, or none, fails the handshake. The proxy is off unless `--proxy` is set. The chart can pin the Service's `clusterIP` (value `proxy.clusterIP`) so `hostAliases` can name it.

### Authentication

As phase 1: one `Authorization` header (`Bearer`, `token`, or Basic password = ServiceAccount token). git sends Basic; `gh` sends `token`. No credential → `401` with `WWW-Authenticate: Basic realm="GitHub"` (so git asks its credential helper). The client's credential is always removed before forwarding.

### Classifying a request

Before anything is forwarded:

1. **Path hygiene.** Reject (`400`) a path with `.`/`..` segments, empty segments, `%2F`, `%5C`, or percent-encoding of unreserved characters; a query on a path that never takes one is passed only after parsing and re-encoding. The forwarded URL is rebuilt from the parsed parts, never copied.
2. **Host `api.github.com`:**
   - `POST /graphql` → GraphQL (below).
   - anything else → REST: match the catalogue; unknown → `403 unknown_operation`.
3. **Host `uploads.github.com`:** REST against the same catalogue (the upload operations are in it).
4. **Host `github.com`:** only git smart HTTP for `/<owner>/<repo>(.git)/info/refs?service=git-upload-pack|git-receive-pack`, `…/git-upload-pack`, `…/git-receive-pack`. Anything else → `404`.

Result: the repository (if any), the operation id or mutation names, the refs (pushes), and the permission level the request needs (git fetch: `contents: read`; push: `contents: write`; REST/GraphQL: left to GitHub).

### Deciding

The candidate rules are the caller's accepted rules with `proxy.enabled`, for the installation whose owner matches the request's owner (one installation per owner among the candidates, else `409`).

- **Request names a repository** (REST with `{owner}/{repo}`, git): rules covering that repository. None → `403 denied` naming the repository.
- **No repository** (GraphQL, REST without `{repo}`): rules with **exact** repository names (globs can't be resolved without a repository). REST without `{repo}` is allowed only if listed in `allowOperations` of a candidate rule; otherwise `403`.
- **Operations:** for each candidate rule, the request is allowed if it passes that rule's lists; the request is allowed if any candidate rule allows it. GraphQL: every mutation name must be allowed by the same rule.
- **pushRefs:** each updated ref must match `pushRefs` of a rule that also covers the repository; a rule without `pushRefs` allows every ref.
- **Token:** minted as in token mode with repositories = the request's repository (or, without one, every exact-name repository of the candidate rules) and permissions = for one repository the union (highest level) of the covering rules; for several, the lowest level each repository has (phase 1 rule). Same cache.

### Forwarding

`httputil.ReverseProxy` with `Rewrite`: URL rebuilt to the real host, `Authorization: token <installation token>`, `Host` set, hop-by-hop headers and `Cookie` dropped, `X-Forwarded-*` not added. Bodies streamed. Responses passed through except `Set-Cookie`; redirects to other hosts (archives, release assets on GitHub's storage) are passed through unchanged, as their URLs carry no installation token. A response or error body never contains the installation token (it is never echoed by GitHub; the leak search in e2e checks it).

### GraphQL

Body limit 1 MiB, read whole (GraphQL bodies are small). JSON object with `query`, optional `operationName`, `variables`. Refused: a JSON array (batch); unknown top-level keys other than `query`, `operationName`, `variables`, `extensions`; persisted-query `extensions` without `query`; a document that doesn't parse; several operations without `operationName`, or an `operationName` that names none; a subscription. For a mutation, the names are the top-level fields of the selected operation's selection set (aliases ignored; the field name counts). A fragment spread or inline fragment at the top level of a mutation is refused. Queries are allowed.

### git receive-pack

Read pkt-lines up to the flush packet: `<old> <new> <ref>` lines, the first with capabilities after NUL. Limit: 1000 commands, 64 KiB of command section. Check every ref against `pushRefs`, then forward the already-read bytes followed by the rest of the stream. A refusal is answered in git's protocol so git shows it: `200` with a `report-status` that has `ng <ref> githubgate: <reason>` for each ref and `unpack ok`, when the client asked for `report-status`; otherwise `403` with the reason as text.

### Interaction table

Every proxied request runs against one snapshot and one token. What happens when state changes while it is in flight:

| Change during a request | Effect |
|---|---|
| Policy removes the rule | the request already decided continues with its token; the next request is refused |
| Token near expiry in cache | cache hands out tokens with ≥ 10 min left; long git pushes keep the token they started with (GitHub checks it at the start) |
| Replica receives SIGTERM | shutdown sequence from phase 1; in-flight streams get the 20 s deadline |
| Installation id refreshed | the token in hand stays valid; new requests use the new id |
| CA / certificate rotated | new handshakes use the new certificate; open connections keep the old one |
| GitHub returns `401` for the token | not retried; passed to the client as `502 github_unavailable` (the token was minted seconds before, so this is GitHub's problem) |

## Client changes

- `configure --proxy`: sets `credential.https://github.com.helper` to `!githubgate-client git-credential --proxy`, which answers with `username=x-access-token` and the ServiceAccount token (read fresh) as password, and never calls the token API.
- `gh --proxy -- …`: runs `gh` with `GH_TOKEN` set to the ServiceAccount token.
- The CA for the proxy comes from the same `GITHUBGATE_CA_FILE`, which the user guide adds to the system trust (`SSL_CERT_FILE` / `GIT_SSL_CAINFO` for git, `SSL_CERT_FILE` for `gh`).

## Signed commits: `githubgate-client push [<remote> [<branch>]]`

Default remote `origin`, branch the current one. Works in both modes (token mode asks the token API for `contents: write` on the repository; proxy mode sends the requests through the proxy).

1. `git fetch <remote> <branch>`; the range is `<remote>/<branch>..HEAD` (if the remote branch doesn't exist: create it from the merge base with the remote's default branch). Refuse if `HEAD` doesn't contain the remote branch (needs a rebase).
2. Refuse, naming the commit, if any commit in the range has more than one parent, or changes a submodule (`160000`). Nothing is published.
3. For each commit, oldest first: create blobs for changed files (`POST /git/blobs`, base64), a tree with `base_tree` = the previous published tree and entries for changes (mode `100644`/`100755`/`120000`; deletions as `sha: null`), and a commit (`POST /git/commits`) with the message plus a `Co-authored-by: <name> <email>` trailer for the original author (skipped if the message already has it), the tree, and the previous published commit as parent. **No `author` or `committer` field** (that is what gets it signed).
4. Move the branch once at the end: `PATCH /git/refs/heads/<branch>` with `force: false` (or `POST /git/refs` to create). If the remote moved meanwhile, nothing is changed and `push` says so.
5. `git fetch`, check that the last published commit's tree equals the local `HEAD` tree, then `git update-ref refs/heads/<branch> <last published> <old HEAD>`. The trees are equal, so the index and working tree need no change.

Large files: blobs over 100 MB are refused with GitHub's limit named.

## Fake GitHub additions

The REST operations and GraphQL mutations the scenarios use (`createPullRequest`, issue comments, releases, release asset upload on an `uploads` host, archive redirect), git receive-pack with `report-status`, and the Git Database endpoints (`blobs`, `trees` with `base_tree` and deletions, `commits` with the signing rule above: a commit with an author field is unsigned, without one it is verified with the App as author, `refs`). The fake serves the three host names with a test CA so e2e pods use real `hostAliases`.

## Testing

- Unit: classification and decision tables (paths, operation matching, GraphQL shapes from scenario 10, pkt-line parsing including malformed input), exhaustive.
- Fuzz: the path parser and the pkt-line reader (`go test -fuzz` targets run in CI for 30 s each).
- e2e: proxied pods with `hostAliases` to the proxy and the fake behind it.
- Live (operator): scenarios 1, 6, 9, 12, 18 on github.com; 18 checks `verification.verified`.

## Issue order

1. CRD `proxy` fields, webhook, model modes; operation catalogue (parallel)
2. fake GitHub additions (parallel)
3. proxy listener, TLS, auth, classification (REST, git) ; GraphQL classification (parallel, need 1)
4. decision + forwarding (needs 3), receive-pack refs (needs 3)
5. client `--proxy` and `push` (push needs only 2)
6. chart, e2e, docs, live
