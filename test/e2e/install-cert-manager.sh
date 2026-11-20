#!/usr/bin/env bash
# Installs cert-manager, which the chart needs for its certificates, on the
# kube context E2E_KUBE_CONTEXT and waits until it is ready. It needs none of
# the e2e images, so up.sh runs it while they build. The images are pinned by
# digest; scripts/ci/e2e-images.sh lists them for the preload.
set -euo pipefail

context="${E2E_KUBE_CONTEXT:?E2E_KUBE_CONTEXT is required}"

chart_version="1.21.3"
args=()
set_image() {
	local prefix="$1" ref="$2" repo digest
	digest="${ref#*@}"
	repo="${ref%%[:@]*}"
	args+=(--set "${prefix}.repository=${repo}" --set "${prefix}.digest=${digest}")
}
set_image image "quay.io/jetstack/cert-manager-controller:v1.21.3@sha256:9f4f96a33c7774cef24466fcd85031997531a03a8d117f151126baa8d1290dcc"
set_image cainjector.image "quay.io/jetstack/cert-manager-cainjector:v1.21.3@sha256:083d0e906f8eb44e255fe4ff817ae54e49f9ed82e22f1011bb4e9e4f4b8a641f"
set_image webhook.image "quay.io/jetstack/cert-manager-webhook:v1.21.3@sha256:055ac8e9de6a0ba6763794838fe38944acc650f68ebe1b01dd8be079e580f458"
set_image startupapicheck.image "quay.io/jetstack/cert-manager-startupapicheck:v1.21.3@sha256:1c519d4afca1413d1e4ccd78bdc756184418ab55a34569a3b3de39d32f9974e1"

helm --kube-context "$context" upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
	--version "$chart_version" --namespace cert-manager --create-namespace \
	--set crds.enabled=true "${args[@]}" --wait --timeout 4m
