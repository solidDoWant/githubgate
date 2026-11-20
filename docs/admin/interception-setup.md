# Set up interception

For: admins who already run githubgate in proxy mode. You know Kubernetes, Helm and cert-manager. See [Set up proxy mode](proxy-mode-setup.md) first.

Interception lets pods and VMs use `gh`, `git` and `curl` with no setup. In a pod, an injection webhook adds a helper container, the interceptor. In a VM, a NixOS module runs it as a service. Workload users read [Use GitHub with no setup](../user/interception.md).

## Turn on the injection webhook

1. Put the proxy CA certificate in a ConfigMap in the githubgate namespace, under `ca.crt`. It is the CA that signs the proxy certificate.
2. Set the chart values. The webhook needs cert-manager and `proxy.enabled`.

```yaml
inject:
  enabled: true
  image:
    digest: sha256:... # the githubgate-client image digest
  proxyCAConfigMap: githubgate-proxy-ca
```

| Value | Meaning |
|---|---|
| `inject.enabled` | Serve the injection webhook. |
| `inject.image.digest` | Digest of the `githubgate-client` image that the helper runs. Required. |
| `inject.proxyAddress` | The proxy address pods use. Default: `githubgate-proxy.<release namespace>.svc:443`. |
| `inject.proxyCAConfigMap` | The ConfigMap with the proxy CA (`ca.crt`). Required. githubgate reads it again when it changes. |
| `inject.extraCAConfigMap` | A ConfigMap with extra CA certificates (`ca.crt`), for a private CA. Every injected pod trusts them. |

The [chart values](chart-values.md) page lists all values.

For one namespace, tenants or you can name a ConfigMap of extra CAs in the annotation `githubgate.soliddowant.dev/extra-ca-configmap`. The ConfigMap must be in that namespace, with the key `ca.crt`.

3. Label each namespace that opts in:

```
kubectl label namespace team-a githubgate.soliddowant.dev/inject=true
```

The webhook never runs in the githubgate namespace.

## It fails closed

The webhook has `failurePolicy: Fail`. If it is not available, pods in labelled namespaces are not created. This includes pods with the opt-out annotation `githubgate.soliddowant.dev/inject: "false"`.

If the proxy CA ConfigMap is missing or has no certificate, the webhook is not ready. It denies every pod in opted-in namespaces with `githubgate: injection isn't configured (proxy CA)`. It never lets a pod through unchanged. Fix the ConfigMap to recover.

## Do these things when you opt in

- **Restart existing pods.** Pods that exist when you add the label are not changed.
- **Add proxy rules first.** Workloads that use only token rules get `403` after they opt in, because the proxy decides every request by proxy rules. Add `proxy.enabled: true` rules to their policies before you label the namespace. See [Set up proxy mode](proxy-mode-setup.md).
- **Rotate the proxy CA with restarts.** A pod gets the CA when it is created. After you rotate the proxy CA, running pods fail with `503` until they restart.

## VM workspaces

The flake exports `nixosModules.githubgate-client`. Add it to the NixOS configuration of the VM image:

```nix
{
  imports = [ inputs.githubgate.nixosModules.githubgate-client ];

  services.githubgate-client = {
    enable = true;
    package = inputs.githubgate.packages.${pkgs.system}.githubgate-client; # any package with bin/githubgate-client
    proxyAddress = "githubgate-proxy.githubgate.svc:443";
    proxyCA = "/etc/githubgate/proxy-ca.crt";
    extraCA = "/etc/githubgate/extra-ca.crt"; # optional: private CAs
    # tokenFile = "/var/run/secrets/githubgate/token"; # the default
  };
}
```

| Option | Meaning |
|---|---|
| `enable` | Run the interceptor service. |
| `package` | A package with `bin/githubgate-client`. Required. |
| `proxyAddress` | The `host:port` of the proxy. The VM must reach it. |
| `proxyCA` | A file with the proxy CA certificate. |
| `extraCA` | A file with extra CA certificates that the VM's tools trust. Default: none. |
| `tokenFile` | The file with a ServiceAccount token for the audience `githubgate`. The service user must read it. |

The module runs the service as its own user, maps the three GitHub names to `127.0.0.1`, and sets the trust variables for the whole system. The VM's own CAs stay in the trust bundle. Before the service starts, requests for GitHub are refused, and other hosts keep working.

You must give the VM image a token file, and the proxy address must be reachable from the VM. Both come from your own VM image setup.
