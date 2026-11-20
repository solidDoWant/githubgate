# githubgate for developers

For: people who change the githubgate code. You know Go and controller-runtime.

githubgate is in early development. The pages on the architecture are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- how a token request is resolved against the policies and the ceilings;
- the fake GitHub in `test/fakegithub`, and which GitHub behaviours it copies;
- the test tiers: unit tests, integration tests (envtest with githubgate and the fake GitHub in one process), e2e tests (kind, the fake GitHub and the chart) and live tests (the owner's test App on github.com);
- how acceptance tests are tied to scenarios (the `Tests:` lines of the checklist) and counted by `make acceptance`;
- how to release.

## Test tiers

- `make test` runs the unit tests, and the tests of the agent and CI scripts.
- `make test-integration` runs the `integration` tests. They need only the dev shell: envtest gets its binaries from `KUBEBUILDER_ASSETS`.
- `make test-e2e` runs the `e2e` tests on the kind cluster `E2E_CLUSTER` (default `githubgate-e2e`). The script `test/e2e/up.sh` creates the cluster with the `ImageVolume` feature gate and loads the server and client images. It builds them with `test/e2e/build-images.sh`, unless `E2E_IMAGES_EXIT_FILE` names a file where another process writes the build's exit status. CI builds the images while the cluster is made. The tests install the fake GitHub and the chart. The cluster is deleted afterwards, unless you set `KEEP_CLUSTER=1`. Without Docker the tests skip.
- `make test-live` runs the `live` tests against github.com with the owner's test App. Only the operator runs them, by hand and never in CI. The App's private key is in the file named by `GITHUBGATE_LIVE_KEY_FILE`; the tests skip without it. The test also needs `GITHUBGATE_LIVE_APP_ID`, `GITHUBGATE_LIVE_OWNER` and `GITHUBGATE_LIVE_REPO` (a throwaway repository the test App is installed on with contents and issues write, and with at least one commit), plus `git`, `gh` and `go` on the PATH. It removes the releases, tags and branches it creates and closes the issue it opens. The key is never put in the repository.

## Getting started

Run `nix develop` (or let `direnv` load `.envrc`), then `make help` for the full list of commands. The ones you need most:

- `make build`: build `bin/githubgate` and `bin/githubgate-client`.
- `make fmt lint test`: format, run every check and run the unit tests.
- `make generate`: make the generated code and the reference pages again after you change API types, markers, the chart values or the CI template.
