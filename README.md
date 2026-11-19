# githubgate

githubgate gives Kubernetes workloads limited access to GitHub through GitHub Apps. A workload never gets the App's private key. It shows its own ServiceAccount token, and githubgate gives it a short-lived token for only the repositories and permissions its policy allows. Admins write these policies as Kubernetes objects.

**Status:** early development. Nothing works yet: the repository has the build, test and CI tooling and a program that prints its version. The first release will be token mode (see the phases in [`SPEC.md`](SPEC.md)).

- [`SPEC.md`](SPEC.md): what githubgate does and the rules it keeps.
- [`docs/`](docs/README.md): the pages for users, admins and developers.

githubgate is licensed under the [GNU Affero General Public License v3.0](LICENSE).
