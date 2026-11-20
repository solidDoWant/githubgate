# Set up interception

For: admins who already run githubgate in proxy mode. You know Kubernetes, Helm and cert-manager. See [Set up proxy mode](proxy-mode-setup.md) first.

Interception lets pods and VMs use `gh`, `git` and `curl` with no setup. An injection webhook adds a helper container, the interceptor, to each opted-in pod. For a KubeVirt VM, the interceptor runs in the VM's launcher pod. Workload users read [Use GitHub with no setup](../user/interception.md).

## Turn on the injection webhook

1. Make sure that the webhook can get the proxy CA, the CA that signs the proxy certificate. By default, the chart takes it from `ca.crt` in the Secret of the proxy certificate. cert-manager writes `ca.crt` for a `ca` issuer. If your Secret has no `ca.crt`, put the CA in a ConfigMap in the githubgate namespace, under `ca.crt`, and set `inject.proxyCAConfigMap`.
2. Set the chart values. The webhook needs `proxy.enabled`. It also needs a certificate for the webhook server, from cert-manager or from `webhook.existingSecret` (see [Certificates](token-mode-setup.md#certificates)).

```yaml
inject:
  enabled: true
  image:
    digest: sha256:... # the githubgate-client image digest
```

| Value | Meaning |
|---|---|
| `inject.enabled` | Serve the injection webhook. |
| `inject.image.digest` | Digest of the `githubgate-client` image that the helper runs. Required. |
| `inject.proxyAddress` | The proxy address pods use. Default: `githubgate-proxy.<release namespace>.svc:443`. |
| `inject.proxyCAConfigMap` | A ConfigMap with the proxy CA (`ca.crt`). Default: `ca.crt` of the proxy Secret. githubgate reads the CA again when it changes. |
| `inject.extraCAConfigMap` | A ConfigMap with extra CA certificates (`ca.crt`), for a private CA. Every injected pod trusts them. |

The [chart values](chart-values.md) page lists all values.

For one namespace, tenants or you can name a ConfigMap of extra CAs in the annotation `githubgate.soliddowant.dev/extra-ca-configmap`. The ConfigMap must be in that namespace, with the key `ca.crt`.

3. Label each namespace that opts in:

```
kubectl label namespace team-a githubgate.soliddowant.dev/inject=true
```

The webhook never runs in the githubgate namespace.

## Opt in single pods

In a namespace where most pods must not be intercepted, do not label the namespace. Label each pod that opts in instead:

```yaml
metadata:
  labels:
    githubgate.soliddowant.dev/inject: "true"
```

The webhook has two entries. One selects pods in labelled namespaces. The other selects labelled pods in all other namespaces, so the webhook gets each pod only once. Both entries skip the githubgate namespace.

## Change the listen address

The interceptor listens on `127.43.0.1:443` by default. To use another address, set this pod annotation:

```yaml
metadata:
  annotations:
    githubgate.soliddowant.dev/listen: "10.0.2.1:443"
```

The value must be a specific IP address with port 443. If it is not, the webhook rejects the pod and names the annotation. The pod `hostAliases` then point the three GitHub names to this IP address. The interceptor binds an address that does not exist yet, and serves it when it appears.

## It fails closed

The webhook has `failurePolicy: Fail`. If it is not available, pods in labelled namespaces are not created. This includes pods with the opt-out annotation `githubgate.soliddowant.dev/inject: "false"`.

If the proxy CA ConfigMap is missing or has no certificate, the webhook is not ready. It denies every pod in opted-in namespaces with `githubgate: injection isn't configured (proxy CA)`. It never lets a pod through unchanged. Fix the ConfigMap to recover.

## Do these things when you opt in

- **Restart existing pods.** Pods that exist when you add the label are not changed.
- **Add proxy rules first.** Workloads that use only token rules get `403` after they opt in, because the proxy decides every request by proxy rules. Add `proxy.enabled: true` rules to their policies before you label the namespace. See [Set up proxy mode](proxy-mode-setup.md).
- **Rotate the proxy CA with restarts.** A pod gets the CA when it is created. After you rotate the proxy CA, running pods fail with `503` until they restart.

## VM workspaces

A KubeVirt VM runs in a pod, the virt-launcher pod. The interceptor runs in that pod, not in the VM. Nothing of githubgate runs in the VM, and the ServiceAccount token never enters the VM. With masquerade networking, the VM uses `10.0.2.1` as its gateway. This address is in the launcher pod. Only the VM and the pod itself can reach it.

Make these changes in the VM template, on the VirtualMachineInstance. KubeVirt copies its labels and annotations to the launcher pod.

1. Add the label `githubgate.soliddowant.dev/inject: "true"`.
2. Add the annotation `githubgate.soliddowant.dev/listen: "10.0.2.1:443"`.
3. Add a `containerPath` volume that shows the directory `/var/run/githubgate/trust` to the VM. The webhook mounts this directory into every container after the interceptor, `compute` included.

Make these changes in the VM image:

1. In `/etc/hosts`, point `api.github.com`, `uploads.github.com` and `github.com` to `10.0.2.1`.
2. Set the trust variables to the files of the shared trust directory, as the webhook does in a pod. See the table of trust variables in [Use GitHub with no setup](../user/interception.md).
3. Set `GH_TOKEN=githubgate`. `gh` refuses to run without a token. The interceptor replaces it.

A VM workspace also reads public repositories of other owners and downloads releases from `github.com`. Interception sends these requests to githubgate too. To let them through, set `publicRead: true` and `webPaths: true` in the `proxy` block of a rule of the workspace's policy. Both settings are off by default:

- `publicRead` sends read-only requests outside the installation with an installation token that has only `metadata: read`. This token reads public data and no private repository contents. It keeps the rate limit of the installation. The rule must name at least one repository without a `*`, because githubgate limits this token to those repositories.
- `webPaths` sends `GET` and `HEAD` requests on `github.com` that are not git with no credential.

Writes outside the installation are still refused. The repositories of the installation still follow the rules. See [Public repositories and web pages](../user/interception.md#public-repositories-and-web-pages).

A program in the VM can send requests through the interceptor. The policy of the launcher pod ServiceAccount limits these requests. The program cannot read the token.

A live migration creates a new launcher pod. The new pod has a new interceptor and a new CA. New programs read the new trust files. A long-running program that loaded the old CA fails on GitHub until you restart it.
