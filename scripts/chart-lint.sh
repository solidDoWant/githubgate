#!/usr/bin/env bash
# Lints the Helm chart without a cluster (`make chart-lint`, part of
# `make lint`): helm lint, then renders with the required values and checks the
# properties the chart promises (hardening, resources).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart=deploy/charts/githubgate
required=(--set "image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000")

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
for want in 'kind: PodDisruptionBudget' 'replicas: 3' 'terminationGracePeriodSeconds: 30' 'name: githubgate-ca' 'kind: Certificate' 'kind: Role$' 'tokenreviews'; do
	grep -qE -- "$want" <<<"$default" || fail "default render lacks: $want"
done
grep -qF 'kind: ValidatingWebhookConfiguration' <<<"$default" && fail "webhook rendered while disabled"
# No cluster-wide Secret access.
helm template githubgate "$chart" "${required[@]}" --show-only templates/clusterrole.yaml | grep -q secrets && fail "ClusterRole grants Secret access"
webhook=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set webhook.enabled=true)
for want in 'kind: ValidatingWebhookConfiguration' 'githubgate-webhook'; do
	grep -qF -- "$want" <<<"$webhook" || fail "webhook render lacks: $want"
done
proxy=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set proxy.enabled=true --set proxy.ca.name=proxy-ca --set proxy.clusterIP=10.96.0.50 --set webhook.enabled=true)
for want in 'name: githubgate-proxy' 'clusterIP: "10.96.0.50"' '- --proxy$' 'api.github.com' 'uploads.github.com' '- github.com' 'name: proxy-ca' 'port: 443'; do
	grep -qE -- "$want" <<<"$proxy" || fail "proxy render lacks: $want"
done
grep -qF 'name: githubgate-proxy' <<<"$default" && fail "proxy rendered while disabled"
helm template githubgate "$chart" "${required[@]}" --set proxy.enabled=true >/dev/null 2>&1 && fail "proxy.enabled without proxy.ca.name rendered"
# Every image is pinned by digest.
grep -E '^ +image:' <<<"$default" | grep -qv '@sha256:' && fail "an image is not pinned by digest"
echo "chart-lint: passed"
