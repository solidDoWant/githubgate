#!/usr/bin/env bash
# Lints the Helm chart without a cluster (`make chart-lint`, part of
# `make lint`): helm lint, then renders with the required values and checks the
# properties the chart promises (hardening, resources).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart=deploy/charts/githubgate
required=()

helm lint --strict "$chart" "${required[@]}"

render() { helm template githubgate "$chart" --namespace githubgate "${required[@]}"; }
fail() { echo "chart-lint: $1" >&2; exit 1; }

default=$(render)
for want in 'runAsNonRoot: true' 'readOnlyRootFilesystem: true' 'allowPrivilegeEscalation: false' '- ALL' 'POD_NAMESPACE'; do
	grep -qF -- "$want" <<<"$default" || fail "default render lacks: $want"
done
# Memory request equals limit, and there is no CPU limit.
mem=$(grep -E '^ +memory:' <<<"$default" | sort -u | wc -l)
[ "$mem" -eq 1 ] || fail "memory request and limit differ"
helm template githubgate "$chart" "${required[@]}" --show-only templates/deployment.yaml |
	awk '/limits:/{f=1;next} f&&/cpu:/{exit 1} /requests:/{f=0}' || fail "a CPU limit is set"
echo "chart-lint: passed"
