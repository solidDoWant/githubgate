# Set up proxy mode

For: admins who already run githubgate in token mode and want to turn on proxy mode. You know Kubernetes, Helm, cert-manager and GitHub Apps. See [Set up token mode](token-mode-setup.md) first.

In proxy mode, githubgate answers for `api.github.com`, `uploads.github.com` and `github.com`. Pods reach it with `hostAliases` and trust a CA that you make. githubgate checks each request against the rules of the caller. Then it sends the request to GitHub with an installation token. The pod never holds that token.

## 1. Make a name-constrained CA

The proxy certificate names real GitHub hosts. A pod that trusts the CA would trust any certificate the CA signs. So limit the CA to the GitHub names with an X.509 name constraint. Use one permitted DNS subtree, `github.com`. That subtree permits `github.com` and every subdomain of it, not only `api.github.com` and `uploads.github.com`. githubgate serves only those two hosts and `github.com`: the proxy refuses any other name. But a pod that trusts the CA trusts any certificate the CA signs for any subdomain of `github.com`. For this reason, keep the CA key away from everything except githubgate's issuer.

Use a CA that only githubgate's issuer uses. Do not reuse the cluster CA. Make the CA with your own tools. For example, with `openssl`, set this in the CA extensions:

```
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign
nameConstraints = critical, permitted;DNS:github.com
```

Then create a cert-manager `Issuer` in the githubgate namespace that uses this CA, as a `ca` issuer with a Secret that holds the key pair. Keep the CA key off the pods.

A client that checks name constraints refuses a certificate from this CA for any other name. Test this before you trust the CA widely.

## 2. Turn on the proxy in the chart

```yaml
proxy:
  enabled: true
  clusterIP: 10.96.0.50 # a free address in the Service range, so pods can name it
  ca:
    name: githubgate-proxy-ca # the Issuer from step 1
    kind: Issuer
    group: cert-manager.io
```

The chart makes the Service `githubgate-proxy` (port 443) and a cert-manager certificate for the three hosts. If you do not use cert-manager, make the certificate yourself and set `proxy.existingSecret` instead of `proxy.ca`. The Secret holds `tls.crt`, `tls.key` and `ca.crt` (see [Certificates](token-mode-setup.md#certificates)). The proxy refuses a TLS connection for any other name. Set `proxy.upstreams.*` only to point at a test double or GitHub Enterprise Server. The [chart values](chart-values.md) page lists every value. The proxy listens on port 9443. If you also use the webhook, give one of them another port (see [Server flags](README.md#server-flags)).

## 3. Give the CA to the tenants

Put the CA certificate (only the certificate) in a ConfigMap as `ca.crt` in each tenant namespace. Tenants mount it at `/var/run/secrets/githubgate-proxy/ca.crt`. Tell them the cluster IP. See [Use proxy mode](../user/proxy-mode.md).

## 4. Write a rule with proxy limits

A rule turns on proxy mode with `proxy.enabled: true`. The rule lists repositories and permissions like in token mode, and GitHub enforces them. The `proxy` block can add finer limits:

```yaml
apiVersion: githubgate.soliddowant.dev/v1alpha1
kind: GitHubAccessPolicy
metadata:
  name: agents
  namespace: team-a
spec:
  serviceAccounts:
    - agent
  rules:
    - name: agent-work
      installationRef:
        namespace: githubgate
        name: acme
      repositories: ["foo"]
      permissions:
        contents: write
        pull_requests: write
      proxy:
        enabled: true
        pushRefs:
          - refs/heads/agent/**
        denyOperations:
          - repos/create-release
        denyMutations:
          - mergePullRequest
```

| Field | What it does |
|---|---|
| `denyOperations` | REST operation ids that githubgate refuses. Use the operation ids of the GitHub REST description, for example `repos/create-release`. |
| `allowOperations` | When set, only these REST operations are allowed. |
| `denyMutations` | GraphQL mutation names that githubgate refuses, for example `mergePullRequest`. |
| `allowMutations` | When set, only these GraphQL mutations are allowed. GraphQL queries are always allowed within the permissions. |
| `pushRefs` | When set, every ref that a request writes must match one of these patterns. githubgate checks a `git push`, the REST calls that write refs, tags and releases, and the GraphQL ref mutations. If githubgate cannot check a ref that a request can write, it refuses the request. For example, it refuses every new deployment, because GitHub can merge the default branch into the ref of the deployment. It also refuses a release update that publishes a draft release without `tag_name`, because GitHub then creates the tag of the draft. |

Rules for these fields:

- With no allow list, every operation that the permissions allow is allowed, except the ones in the deny list. With an allow list, only those are allowed. Deny always wins.
- A list is `1` to `256` entries. An empty list is not valid.
- In operation ids and mutation names, `*` matches one or more characters inside one segment. In refs, `*` matches inside one path segment, and `**` matches one or more whole segments. `refs/heads/agent/**` matches `refs/heads/agent/x` and `refs/heads/agent/x/y`, but not `refs/heads/agent`.
- Every entry must match a known operation, mutation or ref form. An entry that matches nothing is invalid. If a catalogue update leaves an entry matching nothing, the rule becomes `Accepted=False` with reason `InvalidSpec` and gives nothing.
- A rule with `token.enabled: true` cannot have these limits, because a token in the pod could not enforce them. The CRD rejects it.
- A request that githubgate cannot classify is always refused.
- Only the rules that allow a request count. Another rule that covers the same repository cannot undo a limit.

See [the field reference](../user/crd-reference.md) for every field.

## Signed commits

`githubgate-client push` makes commits that GitHub signs. In proxy mode it needs a rule with `contents: write` for the repository. The proxy checks the `git/create-ref` and `git/update-ref` calls against `pushRefs`. Put the branches that agents may use in `pushRefs`.

## What the audit log shows

Each proxied request writes one line. See [the audit log](README.md#the-audit-log) for the fields.
