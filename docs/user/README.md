# githubgate for workload users

For: people who use GitHub from a Tekton pipeline or a Coder workspace through githubgate. You know `git`, `gh` and your pipeline.

- [Get access in a task or a workspace](getting-access.md): set up `git` and `gh` in Tekton and Coder, and read a refusal.
- [Use proxy mode and signed commits](proxy-mode.md): set up a pod for the proxy, push Verified commits, and read a refusal.
- [The client](client.md): `githubgate-client` commands and settings.
- [Field reference](crd-reference.md): every field of the githubgate resources (generated).

Your admin installs githubgate and decides what your namespace may get. See [`docs/admin/`](../admin/README.md). [`SPEC.md`](../../SPEC.md) has the full rules.
