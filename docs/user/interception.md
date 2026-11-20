# Use GitHub with no setup in your pod

For: people who run `git`, `gh` and `curl` in pods, and whose admin turned on interception. You know pod specs and `git`.

With interception, a pod needs no GitHub setup. `gh`, `git` and `curl` work as they do against GitHub. githubgate adds a small helper container to your pod (the interceptor). It takes every request for `github.com`, `api.github.com` and `uploads.github.com` and sends it to the githubgate proxy. It replaces any credential in the request with your pod's ServiceAccount token. The proxy checks the request against your policy, as in [proxy mode](proxy-mode.md). Your pod never holds a GitHub token.

Your policy needs a rule with `proxy.enabled: true` that names your ServiceAccount. Without it, the proxy refuses every request.

## Opt a namespace in

Your admin turns this on for a namespace with a label:

```
kubectl label namespace team-a githubgate.soliddowant.dev/inject=true
```

A pod that is created after the label gets the helper. A pod that already runs does not change. Restart it.

To keep one pod out, set this annotation on the pod:

```yaml
metadata:
  annotations:
    githubgate.soliddowant.dev/inject: "false"
```

In a namespace without the label, one pod can opt in with the same name as a pod label:

```yaml
metadata:
  labels:
    githubgate.soliddowant.dev/inject: "true"
```

## What a pod gets

- A first init container, `githubgate-intercept`. It runs for the life of the pod (a native sidecar). The other containers start only after it is ready.
- `hostAliases` that point the three GitHub names to the address of the helper, `127.43.0.1` by default. The pod annotation `githubgate.soliddowant.dev/listen` sets another address.
- The sysctl `net.ipv4.ip_unprivileged_port_start=443`, so the helper can listen on port 443. It is not added if the value is already 443 or lower.
- A read-only mount of the trust files at `/var/run/githubgate/trust`, in every container.
- The trust variables in every container (see the table below).
- `GH_TOKEN=githubgate`, only in a container that sets neither `GH_TOKEN` nor `GITHUB_TOKEN`. `gh` refuses to run without a token. The interceptor replaces this one.
- A change to `NO_PROXY` and `no_proxy`. If a container sets an `HTTP_PROXY`, `HTTPS_PROXY` or `ALL_PROXY` variable (upper or lower case), githubgate adds the three GitHub names to both variables, so that GitHub requests skip your egress proxy. githubgate creates the variables if they are missing.

The ServiceAccount token for the helper is a projected token that only the helper can read.

### Private CAs

The trust files hold the default CA roots, the pod's own CA and any extra CAs that your admin set. To add a CA to every pod in your namespace, ask your admin, or put the certificate in a ConfigMap in your namespace under the key `ca.crt` and annotate the namespace:

```
kubectl annotate namespace team-a githubgate.soliddowant.dev/extra-ca-configmap=my-ca
```

If a container already sets `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` or `NODE_EXTRA_CA_CERTS` to a file in one of its own volume mounts, githubgate joins that file with the pod CA. The container's variable then points at the joined file. The helper reads the source again every 30 seconds. A process that already loaded the file keeps the old one. Until the source file exists, the joined file holds only the pod CA, so GitHub works and other hosts fail.

## The variables

| Variable | Set by | Meaning |
|---|---|---|
| `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` | the webhook, in your containers | `/var/run/githubgate/trust/bundle.crt`, the default roots plus the pod CA |
| `NODE_EXTRA_CA_CERTS` | the webhook, in your containers | `/var/run/githubgate/trust/ca.crt`, the pod CA only |
| `GH_TOKEN` | the webhook, only if you set neither `GH_TOKEN` nor `GITHUB_TOKEN` | the text `githubgate`, a placeholder |
| `GITHUBGATE_INTERCEPT_PROXY_ADDRESS` | the webhook, in the helper | `host:port` of the proxy |
| `GITHUBGATE_INTERCEPT_PROXY_CA` | the webhook, in the helper | the proxy CA certificate (PEM text or `@file`) |
| `GITHUBGATE_INTERCEPT_EXTRA_CA` | the webhook, in the helper | extra CA certificates (PEM text or `@file`) |
| `GITHUBGATE_PROXY_ADDRESS` | you, for `--proxy` in [proxy mode](proxy-mode.md) | the proxy IP address |
| `GITHUBGATE_PROXY_HOST` | you, for `--proxy` | the name of the proxy Service |
| `GITHUBGATE_PROXY_CA_FILE` | you, for `--proxy` | the proxy CA file, default `/var/run/secrets/githubgate-proxy/ca.crt` |

## Pods that githubgate rejects

githubgate refuses to create a pod it cannot make safe. The error starts with `githubgate: pod can't be intercepted:` and gives the reason. A pod is rejected when:

- it sets `hostNetwork: true`. The pod would not use its own loopback address and `hostAliases`.
- its `hostAliases` name `github.com`, `api.github.com` or `uploads.github.com`. They would override the redirect.
- a container uses port `443`. The helper needs it.
- the pod sets the sysctl `net.ipv4.ip_unprivileged_port_start` above 443. The helper could not listen.
- a container sets one of the CA variables to a file in the image, or through `valueFrom`. githubgate cannot read that file. Put the CA in a ConfigMap and name it in the namespace annotation `githubgate.soliddowant.dev/extra-ca-configmap`.
- the namespace annotation names a ConfigMap that does not exist.
- the annotation `githubgate.soliddowant.dev/listen` is not a specific IP address with port 443, for example `10.0.2.1:443`.
- it already uses a name that githubgate reserves: a container `githubgate-intercept`, or a volume or mount `githubgate-token`, `githubgate-trust` or `githubgate-intercept-state`.

If a pod is not created and the message says `injection isn't configured`, tell your admin. See [Set up interception](../admin/interception-setup.md).

## What interception does not cover

- A tool that resolves names without `/etc/hosts` does not reach the helper.
- Proxy variables that a process sets at run time, and a `NO_PROXY` or `no_proxy` set through `valueFrom`, are not changed. Such a tool may send GitHub requests to your egress proxy.
- In these cases, only a credential that the pod has by itself can reach the real GitHub. The ServiceAccount token cannot, because only the helper reads it and it sends the token only to a server that the proxy CA verifies.
- A process that loaded a CA file keeps it. It does not see a later change until it restarts.
- When your admin rotates the proxy CA, running pods fail with `503` until you restart them.
- A KubeVirt VM gets a new CA when it moves to another node (live migration). A program that loaded the old CA, for example a Node process that read `NODE_EXTRA_CA_CERTS` once, fails on GitHub until you restart it.

## Remove old setup

When your namespace opts in, remove the earlier setups:

- **Token mode** (`githubgate-client gh`, `githubgate-client git-credential`, see [Get GitHub access](getting-access.md)): the helper strips the installation token and uses your ServiceAccount token. The proxy then decides the request by your **proxy** rules. A policy that has only token rules gets `403`. Ask your admin to add proxy rules first.
- **Proxy mode** with `--proxy` (see [Use proxy mode](proxy-mode.md)): remove it, with its `hostAliases`, CA mount and token volume. The client stops, because the three names now resolve to the address of the helper.
