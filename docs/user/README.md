# githubgate for workload users

For: people who use GitHub from a Tekton pipeline or a Coder workspace through githubgate. You know `git`, `gh` and your pipeline.

githubgate is in early development. These pages are not written yet. They will show:

- how to get a GitHub token in a Tekton task, and how to use `git` and `gh` there;
- how to use `git` and `gh` in a Coder workspace;
- why a request was denied, and what to ask your admin for.

- [The client](client.md): `githubgate-client` commands and settings.
- [Field reference](crd-reference.md): every field of the githubgate resources (generated).

Your admin installs githubgate and decides what your namespace may get. See [`docs/admin/`](../admin/README.md). [`SPEC.md`](../../SPEC.md) has the full rules.
