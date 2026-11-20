# Set up token mode

For: admins who give workloads GitHub access. You know Kubernetes, Helm and GitHub Apps.

This page walks through the setup: the chart, a GitHub App, its installation with a ceiling, and a policy for a namespace. Then it shows how to read the status.

## 1. Install githubgate

Install the chart in the namespace that holds your admin objects.

```
helm install githubgate deploy/charts/githubgate --namespace githubgate --create-namespace
```

Set `adminNamespaces` to the namespaces where you will create `GitHubApp` and `GitHubInstallation` objects. The default is the release namespace. githubgate reads Secrets only there. It ignores these two kinds anywhere else. Set `webhook.enabled` to reject them at creation instead. See [chart values](chart-values.md).

### Certificates

githubgate serves TLS (encrypted HTTP) with certificates that it reads from Secrets. Each Secret holds `tls.crt` and `tls.key`. githubgate reads a Secret again when it changes. githubgate never makes a certificate itself. You choose one of two modes.

In the default mode, `certManager.enabled: true`, cert-manager makes the certificates. You must install cert-manager first. The chart creates these objects:

- A certificate authority (CA), as a cert-manager `Certificate` with its key in the Secret `<release name>-ca`. A CA is the certificate that signs the server certificates. Name constraints limit it to the names `githubgate.<namespace>.svc` and `githubgate.<namespace>.svc.cluster.local`.
- The certificate of the token API, signed by that CA, in the Secret `<release name>-tls`.
- The certificate of the webhook server, if a webhook is on. cert-manager's cainjector puts the CA in the `caBundle` of the webhook configurations.
- The proxy certificate, if proxy mode is on. The issuer that you name in `proxy.ca` signs it. See [Set up proxy mode](proxy-mode-setup.md).

No key or certificate is in the rendered manifests or in the Helm release.

In the existing-Secret mode, `certManager.enabled: false`, you make each certificate and put it in a Secret in the release namespace. The chart then creates no Issuer, no `Certificate` and no Secret.

```yaml
certManager:
  enabled: false
tls:
  existingSecret: githubgate-api-tls # tls.crt and tls.key for githubgate.<namespace>.svc
webhook:
  enabled: true
  existingSecret: githubgate-webhook-tls
  caBundle: LS0tLS1CRUdJTi... # base64 PEM of the CA that signs the webhook certificate
proxy:
  existingSecret: githubgate-proxy-tls # tls.crt, tls.key and ca.crt, when proxy.enabled is true
```

| Value | Meaning |
|---|---|
| `tls.existingSecret` | The Secret with the certificate of the token API. Required in this mode. |
| `webhook.existingSecret` | The Secret with the certificate of the webhook server. Required when `webhook.enabled` or `inject.enabled` is true. |
| `webhook.caBundle` | The CA of the webhook certificate, as base64 PEM. The chart puts it in `caBundle`. |
| `webhook.annotations` | Annotations on the webhook configurations. Use it instead of `webhook.caBundle` if cert-manager's cainjector keeps `caBundle` current, for example `cert-manager.io/inject-ca-from: githubgate/my-webhook-certificate`. |
| `proxy.existingSecret` | The Secret with the proxy certificate. Required when `proxy.enabled` is true. |

In this mode, the chart needs `webhook.caBundle` or `webhook.annotations` for a webhook. Without one of them, the API server cannot trust the webhook, so the chart refuses to render. You can also give one Secret in the default mode, for example only `tls.existingSecret`. cert-manager then makes the other certificates.

### Give the CA to clients

Clients need the CA of the token API to trust githubgate. They mount it from a ConfigMap at `/var/run/secrets/githubgate/ca.crt`. You must put that ConfigMap in each client namespace, for example with trust-manager or Kyverno.

- In the default mode, the CA is in the Secret `<release name>-ca`, key `ca.crt`. Copy only `ca.crt`. Do not copy `tls.key`.
- In the existing-Secret mode, use the CA that signed `tls.existingSecret`.

The chart can also create a ConfigMap with the CA in the release namespace. Set `certificate.caConfigMap.create: true` and put the CA as PEM in `certificate.caBundle`. The ConfigMap is named `githubgate-ca` (`certificate.caConfigMap.name`). The chart never fills it from a Secret.

If the CA changes, update the ConfigMaps. Clients that already read the old CA must read the new one again, so restart them.

## 2. Create a GitHub App

1. In GitHub, create a GitHub App. Give it only the repository permissions that you will ever allow, for example `contents` and `pull_requests`. Do not add organization or account permissions. githubgate does not accept them.
2. Install the App on the user or organization that owns the repositories.
3. Note the App ID and generate a private key.
4. Store the key in a Secret in an admin namespace:

```
kubectl -n githubgate create secret generic ci-app --from-file=private-key.pem=key.pem --from-literal=app-id=12345
```

5. Create the `GitHubApp`:

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubApp
metadata:
  namespace: githubgate
  name: ci-app
spec:
  appIDSecretRef:
    name: ci-app
  privateKeySecretRef:
    name: ci-app
  allowedNamespaces: {}
```

You can use `appID` with the number instead of `appIDSecretRef`. The key never leaves githubgate. It only signs requests to GitHub. `allowedNamespaces` says which namespaces may create installations of this App. The namespace of the App is always allowed.

## 3. Create the installation and its ceiling

A `GitHubInstallation` names the App and the owner. githubgate finds the installation id itself. The ceiling is the most that any policy can get through this installation.

```yaml
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
    - repositories: ["*"]
      permissions:
        contents: write
        pull_requests: write
  allowedNamespaces:
    selector:
      matchLabels:
        githubgate.home.arpa/client: "true"
```

Repositories are names under the owner and can use `*` globs. Permission levels are `read`, `write` and `admin`. A higher level includes the lower ones. `allowedNamespaces` says which namespaces may reference this installation from a policy. Label the namespaces of your tenants to match.

## 4. Write a policy for a namespace

A policy lives in the namespace of the workload. It names ServiceAccounts of that namespace and the rules that apply to them.

```yaml
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
      repositories: [foo]
      permissions:
        contents: read
      token:
        enabled: true
```

A rule is accepted only if the ceiling contains every repository and permission in it. Rules have no order and only add access. Policies in other namespaces cannot use the ServiceAccounts of this namespace. Give each pipeline its own ServiceAccount and policy, so a pull request build gets `contents: read` and a release gets `contents: write`.

## 5. Read the status

Run `kubectl get githubapps,githubinstallations,githubaccesspolicies -A`. Each object has a `Ready` condition with a reason. See [the field reference](../user/crd-reference.md) for all fields.

| Reason | What it means | What to do |
|---|---|---|
| `SecretUnavailable`, `SecretInvalid` | The key Secret is missing or is not an RSA key. githubgate refuses requests through the App. | Fix the Secret. It works again without a restart. |
| `InstallationNotFound` | The App is not installed on the owner. | Install the App on the owner. |
| `InstallationSuspended` | GitHub suspended the installation. | Unsuspend it in GitHub. |
| `NotAllowed` | An `allowedNamespaces` list does not admit the referrer. | Change the list or the namespace labels. |
| `RefNotFound`, `RefNotReady` | A referenced object is missing or not ready. | Fix the reference. |
| `GitHubUnavailable` | GitHub cannot be reached. | Wait, then look at the log. |
| `RulesNotAccepted` | A policy has rules that are not accepted. | Read `status.rules`. |

A rule that is outside the ceiling shows `Accepted=False` with the reason `OutsideCeiling`. githubgate ignores that rule and still applies the others. The status of an installation also shows whether the ceiling is within the permissions that the App itself has.

When a user reports a refusal, find the audit line of the request. It shows the caller, the rules that matched, the decision and the reason. See [the admin overview](README.md) for the audit fields and the metrics.

A policy change applies to new tokens within seconds. Tokens that githubgate already gave out stay valid until they expire, at most one hour.
