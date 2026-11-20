# Phase 3 design: transparent interception

Implements SPEC.md §4 "Transparent interception" for `docs/acceptance/phase-3.md`. Builds on `docs/design/phase-1.md` and `docs/design/phase-2.md`; everything there still holds unless this file says otherwise. Where this file and SPEC.md disagree, SPEC.md wins and this file is fixed.

## Overview

The proxy from phase 2 stays the only place that decides and holds tokens. Phase 3 adds a small forwarder next to each workload, the **interceptor** (`githubgate-client intercept`), so the workload's tools need no setup:

```
gh / git / curl ──TLS (pod CA)──▶ interceptor on 127.0.0.1:443 ──TLS (proxy CA)──▶ githubgate proxy ──▶ GitHub
                                   replaces any credential with
                                   the pod's ServiceAccount token
```

In pods, a mutating admission webhook served by githubgate adds the interceptor as a native sidecar. In Coder VM workspaces, a NixOS module runs it as a system service.

## Libraries

| Need | Library |
|---|---|
| Per-pod CA and leaf certificate | `crypto/x509`, `crypto/ecdsa` (P-256) from the standard library |
| System roots in a `FROM scratch` image | `golang.org/x/crypto/x509roots/fallback` (the bundle is written out, see "Trust") |
| Forwarding | `net/http/httputil.ReverseProxy` with `Rewrite` |
| Admission | `sigs.k8s.io/controller-runtime/pkg/webhook/admission` |
| JSON patches | built by marshalling the mutated pod with controller-runtime's `admission.PatchResponseFromRaw` (no hand-written patches) |

## The interceptor

`githubgate-client intercept` flags (env equivalents in brackets):
- `--listen` (default `127.0.0.1:443`);
- `--proxy-address` [`GITHUBGATE_PROXY_ADDRESS`], the proxy Service's host:port;
- `--proxy-ca` [`GITHUBGATE_PROXY_CA`], PEM text or `@file`;
- `--token-file` (default `/var/run/secrets/githubgate/token`);
- `--trust-dir` (default `/var/run/githubgate/trust`), where it writes the trust files.

At start:
1. Generates a CA key (P-256) and a self-signed CA certificate valid for 30 days, `BasicConstraints CA:true, pathlen 0`, **name constraints**: permitted DNS subtrees `api.github.com`, `uploads.github.com`, `github.com`, permitted no IP addresses. Generates a leaf key and certificate for exactly those three names, signed by the CA. Both keys stay in memory only.
2. Writes to `--trust-dir`: `ca.crt` (the pod CA only) and `bundle.crt` (system roots from `x509roots/fallback` plus the pod CA), mode `0444`, then a `ready` file.
3. Listens. Readiness: the startup probe (below) checks for the listener.

Restarting the interceptor makes a new CA. Tools that cached the old one fail one TLS handshake and retry; the bundle file is replaced atomically (write and rename).

Per request (HTTP/1.1 and HTTP/2 over TLS, SNI and `Host` must be one of the three hosts, else the handshake fails or `421`):
- every `Authorization`, `Proxy-Authorization` and `Cookie` header from the client is removed;
- `Authorization: Bearer <ServiceAccount token>` is set, the token read from `--token-file` on every request (rotation: scenario 10);
- the request goes to `--proxy-address` over TLS with SNI = the request's host, verified against `--proxy-ca` for that host name; the upstream URL is built from the host table, never the request;
- the response is streamed back unchanged.

The interceptor makes no decisions and never sees an installation token. It logs only start, stop and errors (no headers).

## Trust in the workload

Tools find the pod CA through environment variables pointing at the shared trust directory:

| Variable | Value | Used by |
|---|---|---|
| `SSL_CERT_FILE` | `bundle.crt` | Go (`gh`), OpenSSL-based tools, Python `ssl` |
| `CURL_CA_BUNDLE` | `bundle.crt` | `curl` |
| `GIT_SSL_CAINFO` | `bundle.crt` | `git` |
| `REQUESTS_CA_BUNDLE` | `bundle.crt` | Python `requests` |
| `NODE_EXTRA_CA_CERTS` | `ca.crt` | Node.js |
| `GH_TOKEN` | `githubgate` (a placeholder) | `gh` refuses to run without a token; the interceptor replaces it |

The bundle includes the system roots, so other HTTPS hosts (including GitHub's storage hosts that downloads redirect to) keep working. The real GitHub can't be reached by mistake from an intercepted pod, because `hostAliases` maps the three hosts to `127.0.0.1` for every container.

## Injection webhook

Served by githubgate with `--inject` (chart value `inject.enabled`), path `/inject-pod`, `MutatingWebhookConfiguration` on pod `CREATE`, `namespaceSelector` `githubgate.soliddowant.dev/inject: "true"`, `failurePolicy: Fail` (scenario 8), `reinvocationPolicy: IfNeeded`, `sideEffects: None`, timeout 5 s.

Skipped (pod returned unchanged): annotation `githubgate.soliddowant.dev/inject: "false"`; already injected (the sidecar is present).

Rejected with a message naming the reason (scenario 7): `hostNetwork: true`; `hostAliases` naming any of the three hosts; any container or init container that sets one of the variables in "Trust" itself; a container port `443` on `127.0.0.1`/all addresses; a pod sysctl `net.ipv4.ip_unprivileged_port_start` set above 443.

Changes made:
- `hostAliases`: `127.0.0.1` → the three hosts.
- `securityContext.sysctls`: `net.ipv4.ip_unprivileged_port_start=443` (a safe sysctl) unless already ≤ 443, so the interceptor binds 443 without privileges.
- Volumes: `githubgate-token` (projected ServiceAccount token, audience from `--audience`, `expirationSeconds: 3600`), mounted **only** in the interceptor; `githubgate-trust` (`emptyDir`, medium Memory, 1 MiB).
- Init container `githubgate-intercept` with `restartPolicy: Always` (a native sidecar), placed **first** in `initContainers` (scenario 12: every later container, init or regular, gets interception), image `ghcr.io/soliddowant/githubgate-client@<digest>` (the chart pins it), args `intercept`, env `GITHUBGATE_PROXY_ADDRESS` and `GITHUBGATE_PROXY_CA` (the PEM inline: the CA certificate is public; a CA rotation applies to new pods), `startupProbe`: `tcpSocket` on 443, period 1 s, failure threshold 30 (scenario 9: kubelet starts later containers only after a native sidecar's startup probe succeeds). Resources: requests = limits memory 64 Mi, no CPU limit; `runAsNonRoot`, read-only root filesystem, all capabilities dropped.
- Every other container and init container gets the "Trust" variables and a read-only mount of `githubgate-trust` at `/var/run/githubgate/trust`.

Pods created before the label was added aren't changed; the admin doc says to restart them.

## Coder VM workspaces

The flake exports `nixosModules.githubgate-client`: options `enable`, `proxyAddress`, `proxyCA`, `tokenFile`. It sets:
- a systemd service running `githubgate-client intercept` as a dedicated user with `AmbientCapabilities=CAP_NET_BIND_SERVICE`, restarting on failure;
- `networking.hosts."127.0.0.1"` with the three hosts;
- the trust: the CA is made at runtime, so it can't go into the system bundle that NixOS builds (`security.pki` takes only build-time files). Instead the module sets the "Trust" variables system-wide (`environment.variables`), pointing at the service's `bundle.crt` and `ca.crt`, plus `GH_TOKEN`.

Wiring the module into the owner's VM image, and giving the VM a token with audience `githubgate`, are changes in the owner's infra repository (the operator files them; they don't block this phase). Scenario 13 is checked by `nix flake check` (the module evaluates) and by a CI test that runs the same binary with the module's settings in a container (an `/etc/hosts` entry, the variables, a token file) against the fake GitHub behind the proxy.

## Interaction table

| Change | Effect |
|---|---|
| ServiceAccount token rotated in the projected volume | the next request reads the new token |
| Interceptor restarts | new CA and bundle; in-flight requests fail and tools retry; the bundle file is replaced atomically |
| Proxy replica killed | the interceptor's next connection goes to another replica through the Service |
| Proxy CA rotated | running pods keep the old CA and fail TLS to the proxy until restarted; documented |
| Policy changes | decided at the proxy, as in phase 2 |
| Webhook unavailable | pods in opted-in namespaces aren't created |

## Testing

- Unit: the webhook's decisions (skip, reject, mutate) as a table over pod shapes, including Tekton-like pods with several init containers; the interceptor's header handling; certificate generation (name constraints present, leaf names exact, keys never written).
- e2e (kind): an opted-in namespace with the webhook, the proxy and the fake GitHub; plain pods with `gh`, `git`, `curl` (scenarios 1–12); the webhook scaled to zero for scenario 8; scenario 11 searches pods, events and logs for the key's PEM header.
- Live (operator): scenarios 1, 2 and 4 with the interceptor, proxy and token API in-process, like the phase 2 live proxy test.

## Issue order

1. interceptor (certificates, trust files, forwarding) ; webhook decisions and mutation (parallel)
2. chart (webhook config, flags, client image digest) and the e2e harness (needs 1)
3. NixOS module and its container test (needs interceptor)
4. docs, live
