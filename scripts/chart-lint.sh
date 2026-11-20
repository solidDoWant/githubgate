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
for want in 'kind: PodDisruptionBudget' 'replicas: 3' 'terminationGracePeriodSeconds: 30' 'kind: Certificate' 'kind: Role$' 'tokenreviews'; do
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
grep -qF 'MutatingWebhookConfiguration' <<<"$default" && fail "injection rendered while disabled"
grep -qF -- '--inject' <<<"$default" && fail "inject flags rendered while disabled"
inject=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set inject.enabled=true --set inject.image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111 --set inject.proxyCAConfigMap=proxy-ca-public --set inject.extraCAConfigMap=extra-ca)
for want in 'kind: MutatingWebhookConfiguration' 'path: /inject-pod' 'failurePolicy: Fail' 'reinvocationPolicy: IfNeeded' 'timeoutSeconds: 5' 'sideEffects: None' 'githubgate.soliddowant.dev/inject' 'kubernetes.io/metadata.name' 'NotIn' '"githubgate"' 'inject-ca-from: githubgate/githubgate-webhook' '- --inject$' '- --inject-image=ghcr.io/soliddowant/githubgate-client@sha256:1111' '- --inject-proxy-ca-file=/var/run/githubgate/inject-proxy-ca/ca.crt' '- --inject-extra-ca-file=' 'name: proxy-ca-public' 'name: githubgate-webhook$'; do
	grep -qE -- "$want" <<<"$inject" || fail "inject render lacks: $want"
done
for want in 'name: inject-pod.githubgate.soliddowant.dev' 'objectSelector:'; do
	grep -qF -- "$want" <<<"$inject" || fail "inject render lacks the pod-label entry: $want"
done
[ "$(grep -c 'values: \["githubgate"\]' <<<"$inject")" -eq 2 ] || fail "both inject entries must exclude the release namespace"
grep -qF -- '--inject-proxy-address' <<<"$inject" && fail "inject proxy address set without a value"
helm template githubgate "$chart" "${required[@]}" --set inject.enabled=true --set inject.proxyCAConfigMap=x >/dev/null 2>&1 && fail "inject.enabled without a digest rendered"
helm template githubgate "$chart" "${required[@]}" --set inject.enabled=true --set inject.image.digest=sha256:1 >/dev/null 2>&1 && fail "inject.enabled without proxyCAConfigMap rendered"
# Certificates, default mode: cert-manager makes a name-constrained CA, so no
# key or certificate is in the render (nor in Helm's release record).
grep -qE -- 'BEGIN|tls\.key:|stringData|^kind: Secret$' <<<"$default" && fail "default render holds key or certificate material"
for want in 'selfSigned: \{\}' 'isCA: true' 'secretName: githubgate-ca$' '^  nameConstraints:$' '^    critical: true$' '- githubgate\.githubgate\.svc$' '- githubgate\.githubgate\.svc\.cluster\.local$' 'secretName: githubgate-ca$' 'secretName: githubgate-tls$'; do
	grep -qE -- "$want" <<<"$default" || fail "default render lacks the cert-manager CA: $want"
done
grep -qF 'kind: ConfigMap' <<<"$default" && fail "CA ConfigMap rendered while certificate.caConfigMap.create is false"
grep -qF 'caBundle' <<<"$webhook" && fail "webhook caBundle rendered while cert-manager injects it"
grep -qF 'inject-ca-from: githubgate/githubgate-webhook' <<<"$webhook" || fail "webhook render lacks the cainjector annotation"
annotated=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set webhook.enabled=true --set webhook.annotations.example\\.com/owner=infra)
grep -qF 'example.com/owner: infra' <<<"$annotated" || fail "webhook.annotations not rendered"
# The client CA ConfigMap is opt-in and filled only from a value.
cm=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set certificate.caConfigMap.create=true --set certificate.caBundle=CLIENT-CA-PEM --show-only templates/certificates.yaml)
for want in 'kind: ConfigMap' 'name: githubgate-ca$' 'CLIENT-CA-PEM'; do
	grep -qE -- "$want" <<<"$cm" || fail "CA ConfigMap render lacks: $want"
done
helm template githubgate "$chart" "${required[@]}" --set certificate.caConfigMap.create=true >/dev/null 2>&1 && fail "certificate.caConfigMap.create without certificate.caBundle rendered"
# One certificate from a Secret, the rest from cert-manager.
mixed=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" --set tls.existingSecret=api-tls)
grep -qF 'secretName: githubgate-tls' <<<"$mixed" && fail "token API Certificate rendered with tls.existingSecret"
grep -qF 'secretName: api-tls' <<<"$mixed" || fail "tls.existingSecret not mounted"
# Existing-Secret mode: no cert-manager object, no Secret; the pods mount the
# admin's Secrets, and the webhooks take the CA from webhook.caBundle.
off=(--set certManager.enabled=false --set tls.existingSecret=api-tls)
existing=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" "${off[@]}" --set webhook.enabled=true --set webhook.existingSecret=hook-tls --set webhook.caBundle=Q0EtUEVN --set proxy.enabled=true --set proxy.existingSecret=own-proxy-tls --set inject.enabled=true --set inject.image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111)
grep -qE -- 'cert-manager\.io|^kind: Secret$|^kind: ConfigMap$|BEGIN' <<<"$existing" && fail "existing-Secret render holds a cert-manager object, a Secret or a ConfigMap"
for want in 'secretName: api-tls$' 'secretName: hook-tls$' 'secretName: own-proxy-tls$' '- --inject-proxy-ca-file=/var/run/githubgate/inject-proxy-ca/ca.crt'; do
	grep -qE -- "$want" <<<"$existing" || fail "existing-Secret render lacks: $want"
done
[ "$(grep -c 'caBundle: Q0EtUEVN' <<<"$existing")" -eq 3 ] || fail "every webhook entry must carry webhook.caBundle"
[ "$(grep -c 'secretName: own-proxy-tls' <<<"$existing")" -eq 2 ] || fail "the injection proxy CA must come from the proxy Secret"
viaInjector=$(helm template githubgate "$chart" --namespace githubgate "${required[@]}" "${off[@]}" --set webhook.enabled=true --set webhook.existingSecret=hook-tls --set webhook.annotations.cert-manager\\.io/inject-ca-from=infra/hook)
grep -qF 'cert-manager.io/inject-ca-from: infra/hook' <<<"$viaInjector" || fail "existing-Secret render lacks webhook.annotations"
grep -qF 'caBundle' <<<"$viaInjector" && fail "caBundle rendered without webhook.caBundle"
helm template githubgate "$chart" "${required[@]}" --set certManager.enabled=false >/dev/null 2>&1 && fail "cert-manager off without tls.existingSecret rendered"
helm template githubgate "$chart" "${required[@]}" "${off[@]}" --set webhook.enabled=true --set webhook.caBundle=Q0E= >/dev/null 2>&1 && fail "cert-manager off with the webhook but without webhook.existingSecret rendered"
helm template githubgate "$chart" "${required[@]}" "${off[@]}" --set webhook.enabled=true --set webhook.existingSecret=hook-tls >/dev/null 2>&1 && fail "cert-manager off with the webhook but no CA source rendered"
helm template githubgate "$chart" "${required[@]}" "${off[@]}" --set proxy.enabled=true >/dev/null 2>&1 && fail "cert-manager off with the proxy but without proxy.existingSecret rendered"
# Every image is pinned by digest.
grep -E '^ +image:' <<<"$default" | grep -qv '@sha256:' && fail "an image is not pinned by digest"
echo "chart-lint: passed"
