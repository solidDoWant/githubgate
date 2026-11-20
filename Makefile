SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

PROJECT_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
BIN_DIR := $(PROJECT_DIR)/bin

VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

# The phase whose checklist `make acceptance` scores (docs/acceptance/phase-$(PHASE).md).
PHASE ?= 1

# The kind cluster `make test-e2e` and `make acceptance` use. Agents set E2E_CLUSTER=githubgate-<issue>.
E2E_CLUSTER ?= githubgate-e2e

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Format Go code (gofmt + goimports through golangci-lint).
	golangci-lint fmt ./...

.PHONY: lint
lint: ## Run every linter and check (golangci-lint, go vet, go mod tidy, shellcheck, Python syntax, image references, tracking IDs, chart lint), then fail if any failed.
	@failed=""; \
	golangci-lint run ./... || failed="$$failed golangci-lint"; \
	$(MAKE) --no-print-directory vet || failed="$$failed vet"; \
	go mod tidy -diff || failed="$$failed go-mod-tidy"; \
	shellcheck $$(git ls-files --cached --others --exclude-standard -- '*.sh' 'scripts/agents/sched-status') || failed="$$failed shellcheck"; \
	./scripts/check-python.sh || failed="$$failed python"; \
	./scripts/check-images.py || failed="$$failed images"; \
	./scripts/check-tracking-ids.py || failed="$$failed tracking-ids"; \
	$(MAKE) --no-print-directory chart-lint || failed="$$failed chart-lint"; \
	if [ -n "$$failed" ]; then echo "lint failed:$$failed" >&2; exit 1; fi

.PHONY: vet
vet: ## Run go vet on every package, with and without the integration, e2e and live build tags.
	go vet ./...
	go vet -tags integration ./...
	go vet -tags e2e ./...
	go vet -tags live ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply its fixes.
	golangci-lint run --fix ./...

.PHONY: test
test: ## Run unit tests: Go (race detector; GO_TEST_JSON=file also writes the go test -json events there), the agent scheduler and wait-ci tests, the test guard, the CI measurement script, the acceptance scoreboard and the check scripts' tests.
	./scripts/ci/go-test-json.sh -race -count=1 ./...
	./scripts/agents/scheduler_test.py
	./scripts/agents/wait-ci_test.sh
	./test/e2e/up_test.sh
	./scripts/ci/test-guard_test.py
	./scripts/ci/measure-ci_test.py
	./scripts/ci/changes_test.py
	./scripts/ci/stages-report_test.py
	./scripts/ci/df-percent_test.sh
	./scripts/ci/stale-images_test.sh
	./scripts/ci/changed-files_test.sh
	./scripts/acceptance-scoreboard_test.py
	./scripts/check-images_test.py
	./scripts/check-tracking-ids_test.py
	./scripts/release_test.sh
	./scripts/ci/publish-release_test.sh

.PHONY: test-guard
test-guard: ## Fail if HEAD deletes, unlists or skips a test named on a `Tests:` line of a checklist since its merge base with BASE (default origin/master; CI passes the PR labels).
	./scripts/ci/test-guard.py --base $(or $(BASE),origin/master)

.PHONY: test-integration
test-integration: ## Run the integration-tagged tests (envtest from the dev shell's KUBEBUILDER_ASSETS, the server and the fake GitHub in-process). GO_TEST_JSON=file also writes the go test -json events there.
	./scripts/test-tagged.sh integration

.PHONY: test-e2e
test-e2e: ## Run the e2e-tagged tests with the fake GitHub and the chart on the kind cluster E2E_CLUSTER (default githubgate-e2e, ImageVolume on).
	E2E_CLUSTER="$(E2E_CLUSTER)" ./scripts/test-tagged.sh e2e

.PHONY: test-live
test-live: ## Run the live-tagged tests against github.com with the owner's test App, its private key in the file GITHUBGATE_LIVE_KEY_FILE; they skip without it. Operator only, never in CI.
	./scripts/test-tagged.sh live

.PHONY: acceptance
acceptance: ## Run every test named on a checklist `Tests:` line and print the scoreboard for docs/acceptance/phase-$(PHASE).md (written to build/acceptance/).
	E2E_CLUSTER="$(E2E_CLUSTER)" PHASE=$(PHASE) ./scripts/acceptance.sh

.PHONY: docs-check
docs-check: ## Check the human-facing Markdown: links and anchors (lychee, offline), agent-prose tells, every docs/ page listed in docs/README.md, generated references current. The plain-English style report is separate and advisory.
	./scripts/docs/check.sh

.PHONY: docs-style-report
docs-style-report: ## Print the advisory plain-English style counts for the human-facing pages changed since origin/master. Never fails.
	./scripts/docs/style_report.sh

.PHONY: vulncheck
vulncheck: ## Report known vulnerabilities in Go dependencies and the standard library.
	govulncheck ./...

##@ Code Generation

.PHONY: generate
generate: ## Regenerate committed generated code with controller-gen: deepcopy, CRDs (config/crd/bases), the manager ClusterRole (config/rbac/role.yaml); then the generated docs references.
	./scripts/generate.sh

.PHONY: check-generate
check-generate: ## Fail if `make generate` changes the tree (committed generated code is stale).
	./scripts/check-generate.sh

##@ Build

.PHONY: build
build: ## Build bin/githubgate and bin/githubgate-client (static, CGO_ENABLED=0).
	@mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o "$(BIN_DIR)/githubgate" ./cmd/githubgate
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o "$(BIN_DIR)/githubgate-client" ./cmd/githubgate-client

.PHONY: build-images
build-images: ## Build the images ghcr.io/soliddowant/githubgate and ghcr.io/soliddowant/githubgate-client (tag $(VERSION)) for linux/amd64 and arm64; PUSH_ALL=true also pushes (releases only).
	VERSION="$(VERSION)" PUSH_ALL="$(or $(PUSH_ALL),false)" ./scripts/build-images.sh

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf "$(BIN_DIR)" build

##@ Helm

.PHONY: chart-lint
chart-lint: ## Lint the Helm chart in deploy/charts/githubgate without a cluster.
	./scripts/chart-lint.sh

##@ Release

.PHONY: release
release: ## Dry run of releasing VERSION: check the version and its CHANGELOG.md section, build the binary, image and chart. Publishing is pushing the tag v$(VERSION) (the release-manager's job).
	VERSION="$(VERSION)" ./scripts/release.sh

##@ Dependencies

.PHONY: update-dependencies
update-dependencies: ## Update the Nix flake inputs and Go modules.
	nix flake update
	go get -u ./...
	go mod tidy
