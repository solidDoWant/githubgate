#!/usr/bin/env bash
# Deletes the e2e kind cluster (E2E_CLUSTER, default githubgate-e2e).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
cluster="${E2E_CLUSTER:-githubgate-e2e}"
kind delete cluster --name "$cluster"
rm -f "build/e2e/${cluster}.env"
