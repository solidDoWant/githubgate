# Phase 3 design: transparent interception

Implements SPEC.md §4 "Transparent interception" for `docs/acceptance/phase-3.md`. Builds on `docs/design/phase-1.md` and `docs/design/phase-2.md`; everything there still holds unless this file says otherwise. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed. Revised after the design review on #136.

## Overview

The proxy from phase 2 stays the only place that decides and holds installation tokens. Phase 3 adds a small forwarder next to each workload, the **interceptor** (`githubgate-client intercept`), so the workload's tools need no setup:

```
gh / git / curl ──TLS (per-pod CA)──▶ interceptor on 127.43.0.1:443 ──TLS (proxy CA only)──▶ githubgate proxy ──▶ GitHub
                                       replaces any credential with
                                       the pod's ServiceAccount token
```

A mutating admission webhook served by githubgate adds the interceptor as a native sidecar. The same path serves Coder VM workspaces: the interceptor runs in the VM's virt-launcher pod and listens on the address the guest uses as its gateway, so neither the binary nor the ServiceAccount token ever enters the guest.

## Libraries

| Need | Library |
|---|---|
| Per-pod CA and leaf certificate | `crypto/x509`, `crypto/ecdsa` (P-256), `encoding/pem` from the standard library |
| Default root bundle in a `FROM scratch` image | `golang.org/x/crypto/x509roots/fallback/bundle` (`Roots()` gives the DER certificates, written out with `encoding/pem`) |
| Forwarding | `net/http/httputil.ReverseProxy` with `Rewrite` |
| Admission | `sigs.k8s.io/controller-runtime/pkg/webhook/admission`; patches from `admission.PatchResponseFromRaw` |
| PodSecurity check in tests | `k8s.io/pod-security-admission/policy` |

## The interceptor

Flags, with environment equivalents (names differ from phase 2's client on purpose, see "Names"):

| Flag | Env | Meaning |
|---|---|---|
| `--listen` | | default `127.43.0.1:443`: a loopback address of its own, so it can't collide with a container's `127.0.0.1:443` or catch `localhost` traffic. Must be a specific IP (never `0.0.0.0` or `::`: that would let anything that reaches the pod IP make requests as the pod's ServiceAccount). A non-loopback address is bound with `IP_FREEBIND`, because a KubeVirt launcher pod's guest gateway only appears after the interceptor starts. |
| `--proxy-address` | `GITHUBGATE_INTERCEPT_PROXY_ADDRESS` | the proxy Service's `host:port` |
| `--proxy-ca` | `GITHUBGATE_INTERCEPT_PROXY_CA` | PEM text or `@file`; the only roots used to verify the proxy |
| `--token-file` | | default `/var/run/secrets/githubgate/token` |
| `--trust-dir` | | default `/var/run/githubgate/trust`: files the workload reads |
| `--state-dir` | | default `/var/run/githubgate/state`: the CA key, readable only by the interceptor |
| `--base-bundle` | | PEM file of roots to include; default the `x509roots/fallback/bundle` roots |
| `--extra-ca` | `GITHUBGATE_INTERCEPT_EXTRA_CA` | PEM text or `@file`, appended to the bundle (private CAs, the fake's test CA in e2e) |
| `--probe` | | run the startup check (below) and exit |

**At start:**
1. `--proxy-ca` must contain at least one certificate, or the interceptor exits non-zero. It is never combined with system roots.
2. **CA:** if `--state-dir` holds `ca.key` and `ca.crt` that parse and have more than 7 days left, reuse them; otherwise create a P-256 key and a self-signed CA certificate valid **10 years**, written with mode `0600`. The CA has `BasicConstraints CA:true, pathlen 0`, **name constraints** (critical): permitted DNS subtrees `api.github.com`, `uploads.github.com`, `github.com`; excluded IP ranges `0.0.0.0/0` and `::/0` (an empty permitted IP list would mean unconstrained); `NotBefore` one hour in the past. Because the CA is stable for the life of the pod or VM, restarting the interceptor doesn't invalidate trust that running processes loaded once.
3. **Leaf:** a key and certificate for exactly the three names, valid 30 days, held in memory, re-issued 7 days before expiry.
4. Writes to `--trust-dir`, each atomically (write, then rename): `ca.crt` (the pod CA only) and `bundle.crt` (`--base-bundle` + `--extra-ca` + the pod CA), mode `0444`; then a `ready` file.
5. Listens.

**`--probe`** (the startup probe, an `exec` probe so it runs in the pod's network namespace): exits 0 only if `ready` exists (written after the listener is bound and the leaf certificate is in place) and, for a **loopback** `--listen` address, a TLS dial to it with SNI `api.github.com` verifies against `ca.crt`. For a non-loopback address the dial is skipped: a KubeVirt launcher pod's guest gateway appears only after virt-launcher starts, which kubelet does only after this probe passes, so dialling it would deadlock the pod. The `ready` file alone shows the same code path has bound and is serving. The webhook sets the startup probe's `timeoutSeconds` to 10, longer than the probe's own 5 s dial timeout.

**Per request** (HTTP/1.1 and HTTP/2):
- TLS SNI must be one of the three hosts (the handshake fails otherwise). `Host`, canonicalised with the proxy's rules (lower-case, `:443` allowed, no trailing dot), decides the upstream and must be one of the three (else `421`); it may differ from SNI (HTTP/2 connection reuse across the leaf's names).
- Every `Authorization`, `Proxy-Authorization` and `Cookie` header is removed; `Authorization: Bearer <token>` is set from `--token-file`, read on every request.
- Outbound: URL `https://<GitHub host>`, `out.Host` = the GitHub host, TLS `ServerName` = the GitHub host, `RootCAs` = the `--proxy-ca` pool only, a `DialContext` that always dials `--proxy-address` (never resolves the GitHub name, which `hostAliases` points back at the interceptor), and `Proxy: nil` (egress-proxy variables are ignored).
- Timeouts as the phase 2 proxy: `ReadHeaderTimeout` 10 s, 5 minutes with no bytes moving, upstream dial 10 s, response header 60 s.
- The response is streamed back unchanged.

**Errors** (phase 2's shapes: JSON `{"message": "githubgate: …", "error": "…", "documentation_url": …}` on the API hosts, `text/plain` on `github.com`):

| Condition | Answer |
|---|---|
| token file missing, empty or unreadable | `503 unavailable` "githubgate: can't read the ServiceAccount token"; nothing forwarded |
| proxy unreachable, TLS verification fails, or timeout | `503 unavailable` "githubgate: proxy unavailable" |
| `Host` not one of the three hosts | `421` |

The interceptor makes no decisions and never sees an installation token. It logs only start, stop and errors (no headers).

## Trust in the workload

| Variable | Value | Used by |
|---|---|---|
| `SSL_CERT_FILE` | `bundle.crt` | Go (`gh`), OpenSSL-based tools, Python `ssl` |
| `CURL_CA_BUNDLE` | `bundle.crt` | `curl` |
| `GIT_SSL_CAINFO` | `bundle.crt` | `git` |
| `REQUESTS_CA_BUNDLE` | `bundle.crt` | Python `requests` |
| `NODE_EXTRA_CA_CERTS` | `ca.crt` | Node.js |
| `GH_TOKEN` | `githubgate`, only if the container sets neither `GH_TOKEN` nor `GITHUB_TOKEN` | `gh` refuses to run without a token; the interceptor replaces it |

These replace the image's own bundle, which keeps the default roots for every other host, including GitHub's storage hosts. Private CAs reach the bundle three ways: the chart's `inject.extraCAConfigMap` (cluster-wide), the namespace annotation `githubgate.soliddowant.dev/extra-ca-configmap: <ConfigMap name>` (key `ca.crt`, in the pod's namespace), or a container that already sets one of the variables (joined, below).

**Joining a container's own CA variable.** Other tools set these variables too (registry tooling, images with internal CAs). Rejecting them would make opting a namespace in break ordinary pods, so the webhook joins them where it can. For each container whose `env` sets `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` or `NODE_EXTRA_CA_CERTS` to a literal path `P`:
- **`P` is inside one of that container's volume mounts** (ConfigMap, Secret, projected, emptyDir): the webhook mounts the same volume (same `subPath`) read-only into the interceptor at `/var/run/githubgate/join/<container>/<n>` and passes `--join <container>/<VARIABLE>=<path in the interceptor>`. The interceptor writes `trust/join/<container>/<VARIABLE>`: the container's own file followed by the pod CA (the container's file keeps deciding every other host, as it did before), re-reads the source every 30 seconds and rewrites the result atomically when it changes. The webhook points the container's variable at that file. Until the source exists (a file an earlier init container writes, for example), the joined file holds only the pod CA, so GitHub works and other hosts fail until the source appears.
- **`P` is a file baked into the image, or the variable comes from `valueFrom`:** neither the webhook nor the interceptor can read it, so the pod is rejected: `githubgate: pod can't be intercepted: container "<name>" sets <VARIABLE> to a file githubgate can't read; put the CA in a ConfigMap and name it in the namespace annotation githubgate.soliddowant.dev/extra-ca-configmap`.
- `SSL_CERT_DIR` isn't one of githubgate's variables and is left alone.

**What interception covers, and what it doesn't.** `hostAliases` maps the three hosts to the listen address for every container, and the webhook makes egress-proxy variables skip them (below), so tools reach the interceptor. Cases outside githubgate's control remain: a tool that resolves names without `/etc/hosts`, or proxy variables set at run time. In those cases only the pod's own credential (if it has one) can reach the real GitHub, never the ServiceAccount token, which only the interceptor reads and only sends to a server verified by the proxy CA.

## Injection webhook

Served by githubgate with `--inject` (it starts the webhook server on its own; `--webhook` remains the validating webhook), path `/inject-pod`, `MutatingWebhookConfiguration` on pod `CREATE` with two webhook entries for the same path, so a pod is opted in either way: (1) `namespaceSelector` label `githubgate.soliddowant.dev/inject: "true"`; (2) `objectSelector` pod label `githubgate.soliddowant.dev/inject: "true"` with a `namespaceSelector` that excludes namespaces with that label (so no pod is sent twice). Both exclude the release namespace (`kubernetes.io/metadata.name NotIn [<release namespace>]`, so githubgate's own pods can never depend on it). Pod-level opt-in is for namespaces where most pods shouldn't be intercepted, such as Coder's, and KubeVirt copies a VMI's labels to its launcher pod. Both entries use `failurePolicy: Fail` (scenario 8), `reinvocationPolicy: IfNeeded`, `sideEffects: None`, timeout 5 s.

**Server flags and chart values:**

| Flag | Chart value | Meaning |
|---|---|---|
| `--inject` | `inject.enabled` | serve the injection webhook |
| `--inject-image` | `inject.image.digest` (with the client image repository) | digest-pinned client image, required |
| `--inject-proxy-address` | `inject.proxyAddress` | default `githubgate-proxy.<release namespace>.svc:443` |
| `--inject-proxy-ca-file` | `inject.proxyCAConfigMap` (a ConfigMap `ca.crt` in the release namespace) | required; re-read on change with `certwatcher` |
| `--inject-extra-ca-file` | `inject.extraCAConfigMap` | optional; passed to the interceptor as `--extra-ca` |

If the proxy CA file is missing or holds no certificate, the replica isn't ready and the webhook denies with "githubgate: injection isn't configured (proxy CA)". It never mutates partially and never allows a pod unchanged.

**Skipped** (returned unchanged): annotation `githubgate.soliddowant.dev/inject: "false"`.

**Rejected**, with the message `githubgate: pod can't be intercepted: <reason>` (scenario 7):
- `hostNetwork: true`;
- `hostAliases` naming one of the three hosts (case-insensitive, trailing dot removed);
- a container or init container that sets one of the CA variables in a way that can't be joined (see "Joining a container's own CA variable"; `env` only: `envFrom` loses to the injected `env`, so no ConfigMap lookup is needed);
- a container port `443`;
- a pod sysctl `net.ipv4.ip_unprivileged_port_start` above 443;
- a container named `githubgate-intercept`, or volumes or mounts named `githubgate-token`, `githubgate-trust` or `githubgate-intercept-state`, that the webhook didn't add (no `githubgate.soliddowant.dev/injected` annotation); or that annotation on a pod without the sidecar.

**Mutation is idempotent** (it runs again on reinvocation, after other webhooks): the reject checks run every time; the sidecar exists and is first in `initContainers`; every container and init container after it has the trust env and mount (and none before it); the webhook sets `githubgate.soliddowant.dev/injected: "true"`. Changes:
- **Listen address:** the pod annotation `githubgate.soliddowant.dev/listen: "<IP>:443"` replaces the default `127.43.0.1:443` (a value that isn't a specific IP with port 443 is a rejection). It becomes the interceptor's `--listen`.
- `hostAliases`: the listen IP → the three hosts.
- `securityContext.sysctls`: `net.ipv4.ip_unprivileged_port_start=443` (a safe sysctl) unless already ≤ 443.
- **Egress proxies:** in every container that sets any of `HTTPS_PROXY`, `https_proxy`, `HTTP_PROXY`, `http_proxy`, `ALL_PROXY`, `all_proxy`, append `api.github.com,uploads.github.com,github.com` to `NO_PROXY` and `no_proxy` (creating them if absent).
- Volumes: `githubgate-token` (projected ServiceAccount token, audience from `--audience`, `expirationSeconds: 3600`), `githubgate-intercept-state` (`emptyDir`, medium Memory, 1 MiB), both mounted **only** in the interceptor; `githubgate-trust` (`emptyDir`, medium Memory, 4 MiB).
- Init container `githubgate-intercept`, `restartPolicy: Always` (native sidecar), image from `--inject-image`, args `intercept`, env `GITHUBGATE_INTERCEPT_PROXY_ADDRESS`, `GITHUBGATE_INTERCEPT_PROXY_CA` (the PEM inline; the CA certificate is public; a rotation applies to new pods) and, if configured, `GITHUBGATE_INTERCEPT_EXTRA_CA`. `startupProbe`: `exec ["/githubgate-client", "intercept", "--probe"]`, period 1 s, failure threshold 30 (scenario 9: kubelet starts later containers only after it succeeds). Security context: `runAsUser`/`runAsGroup` 65532, `runAsNonRoot`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`. Resources: memory request = limit 64 Mi, CPU request 10m, no CPU limit.
- Every later container gets the "Trust" variables (`GH_TOKEN` only as stated there; joined variables point at their joined file) and a read-only mount of `githubgate-trust` at `/var/run/githubgate/trust`.
- If the namespace has the `extra-ca-configmap` annotation, its ConfigMap is mounted read-only into the interceptor and passed as `--extra-ca @<file>` (in addition to the chart's). A missing ConfigMap is a rejection naming it, so pods don't start without the CA they asked for.

Pods created before the label was added aren't changed; the admin doc says to restart them.

## Coder VM workspaces

There is no separate VM path. A Coder VM workspace is a KubeVirt VM whose virt-launcher pod gets the interceptor through the same webhook. With masquerade networking (the cluster default, and the Coder template's), the guest's default gateway, `10.0.2.1`, is an address in the launcher pod's own network namespace: the operator checked on 2026-11-20 that a guest reaches a listener on `10.0.2.1:443` in its launcher pod, and that `IP_FREEBIND` binds the address before it exists. It is reachable only from the guest and the pod itself, not from the cluster network.

The VM template (in the owner's infra repo) sets on the VMI, which KubeVirt copies to the launcher pod:
- the label `githubgate.soliddowant.dev/inject: "true"` and the annotation `githubgate.soliddowant.dev/listen: "10.0.2.1:443"`;
- a `containerPath` volume exposing `/var/run/githubgate/trust` (which the webhook mounts into every later container, `compute` included) to the guest.

The guest image (also the owner's) sends the three hosts to `10.0.2.1` in `/etc/hosts` and sets the "Trust" variables (and `GH_TOKEN`) to the shared trust directory. Nothing runs in the guest, and the ServiceAccount token stays in the launcher pod: an agent in the VM can send requests through the interceptor, which the proxy's policy limits, but can't take the token anywhere.

**Live migration** creates a new launcher pod, so a new interceptor and a new CA (the key never lives in a Kubernetes object). New processes pick up the new bundle; long-running ones that loaded the old CA (for example a Node process reading `NODE_EXTRA_CA_CERTS` once) fail on GitHub until restarted. The user doc says so.

These template and image changes are the owner's (the operator files them); they don't block this phase.

## Names

The interceptor's variables are prefixed `GITHUBGATE_INTERCEPT_`. Phase 2's client keeps `GITHUBGATE_PROXY_ADDRESS` (an IP address, for its resolution check) and `GITHUBGATE_PROXY_CA_FILE`. The user docs list both sets in one table.

## Interaction table

| Change | Effect |
|---|---|
| ServiceAccount token rotated in the projected volume | the next request reads the new token |
| Leaf near expiry | re-issued in memory; the CA is unchanged |
| Interceptor restarts | same CA from the state directory; only in-flight connections fail |
| Proxy replica killed | the next connection goes to another replica through the Service |
| Proxy CA rotated | running pods keep the old CA and fail TLS to the proxy (`503`) until restarted; documented |
| Policy changes | decided at the proxy, as in phase 2 |
| Webhook unavailable | pods in opted-in namespaces aren't created |
| Another webhook adds a container after githubgate | reinvocation gives it the trust env and mount, and joins any CA variable it sets |
| A joined CA file changes (ConfigMap update) | the interceptor rewrites the joined file within 30 seconds; processes that loaded it keep the old one |
| Pod uses phase 1's token-mode client (`githubgate-client gh`, `git-credential`) | its installation token is stripped and replaced; the request is decided by **proxy** rules, so token-only policies get `403` (admin doc: add proxy rules before opting in) |
| Pod uses phase 2's `--proxy` client | refused by its resolution check (the hosts resolve to the listen address); remove that setup when opting in |
| KubeVirt live migration | new launcher pod, new CA; new guest processes work, long-running ones need a restart |

## Testing

- **Unit:** the webhook's decisions (skip, reject, mutate, reinvocation) as a table over pod shapes, including Tekton-like pods with several init containers, proxy variables, `GH_TOKEN`/`GITHUB_TOKEN`, and a pod in a `restricted` namespace that must pass the PodSecurity library after mutation; joining (a CA variable inside a ConfigMap mount, with `subPath`, in an emptyDir written later, baked into the image, from `valueFrom`); the interceptor's header handling and error table; certificates (name constraints critical, IP ranges excluded, a leaf with an IP SAN fails `Verify`, leaf names exact, CA reused from the state directory, keys written only there).
- **Scenario 10:** a test in `cmd/githubgate-client` that swaps the token file the way kubelet's atomic writer does (new `..data` target, then rename) and checks the next request's credential at a stub proxy.
- **e2e (kind):** an opted-in namespace with the webhook, the proxy and the fake GitHub, with `inject.extraCAConfigMap` holding the fake's test CA (scenario 5's storage redirect); plain pods with `gh`, `git`, `curl` (scenarios 1–7, 9, 11, 12). Scenario 8: a test-only `MutatingWebhookConfiguration` pointing at a Service with no endpoints for a separate label, removed in `t.Cleanup` (githubgate itself keeps running). Scenario 11: search pods, events and logs for key material, and assert that every file in the trust directory holds only `CERTIFICATE` blocks and that the sidecar's env and args hold no key.
- **Scenario 13:** e2e: a pod in a namespace without the label, opted in by its own label, with the `listen` annotation set to another loopback address, is intercepted there and its `hostAliases` follow; unit: the interceptor binds a non-local address with `IP_FREEBIND` and serves once the address appears (add it to a test network namespace or a dummy interface where the test can, else assert the bind). The real VM check (a guest using `10.0.2.1`) is done by the operator after the owner's template change, and doesn't block the phase.
- **Live (operator), scenario 14:** `gh` and `git` run in a container with `--add-host` for the three names pointing at an interceptor on port 443 in front of the in-process proxy and token API, against github.com.

## Issue order

1. interceptor (certificates, state, trust files, probe, forwarding, errors); webhook decisions and mutation (parallel)
2. chart (flags, values, ConfigMaps, webhook config) and the e2e harness (needs 1)
3. pod-level opt-in, the `listen` annotation and `IP_FREEBIND` (needs 1)
4. docs, live
