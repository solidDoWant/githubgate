#!/usr/bin/env bash
# Prepares the e2e environment on the kind cluster E2E_CLUSTER (default
# githubgate-e2e, created with test/e2e/kind-config.yaml, ImageVolume on): the
# server and client images built for the host architecture and loaded into
# kind. The tests install the fake GitHub and the chart themselves, because the
# chart needs its Secrets first. Prints the env file path; it holds the kube
# context and the image names.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

cluster="${E2E_CLUSTER:-githubgate-e2e}"
node_image="mirror.gcr.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
tag="e2e"
server_image="ghcr.io/soliddowant/githubgate:${tag}"
client_image="ghcr.io/soliddowant/githubgate-client:${tag}"
env_file="build/e2e/${cluster}.env"
mkdir -p build/e2e

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

start=$SECONDS
log() { echo "e2e-up: [+$((SECONDS - start))s] $*" >&2; }

# The cluster needs nothing from the image builds and the builds need nothing
# from the cluster, so create it in the background while the images build. Only
# the image load needs both.
ci_kind_config=/run/ci-kind/bin/ci-kind-config
cluster_pid=""
cluster_log="$work/cluster.log"
if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	log "creating kind cluster $cluster (in the background)"
	if [[ -x "$ci_kind_config" ]]; then
		# ci-kind-config keeps each node's image store between builds, keyed by
		# the cluster name, so it takes the name and image instead of kind.
		{ "$ci_kind_config" --name "$cluster" --image "$node_image" test/e2e/kind-config.yaml | kind create cluster --config=- --wait 120s; } >"$cluster_log" 2>&1 &
	else
		kind create cluster --name "$cluster" --image "$node_image" --config test/e2e/kind-config.yaml --wait 120s >"$cluster_log" 2>&1 &
	fi
	cluster_pid=$!
fi
# A failed build must not leave the cluster creation running.
stop_cluster() {
	if [[ -n "$cluster_pid" ]]; then
		kill "$cluster_pid" 2>/dev/null || true
		wait "$cluster_pid" 2>/dev/null || true
	fi
}
trap 'stop_cluster; rm -rf "$work"' EXIT

# CI builds the images once for both tracks, at the same time as this script
# runs, and writes the build's exit status to E2E_IMAGES_EXIT_FILE when it ends
# (its output is in E2E_IMAGES_LOG). A developer machine builds them here.
wait_for_images() {
	if [[ -z "${E2E_IMAGES_EXIT_FILE:-}" ]]; then
		log "building the images"
		./test/e2e/build-images.sh
		log "images built"
		return
	fi
	log "waiting for the images to build"
	while [[ ! -s "$E2E_IMAGES_EXIT_FILE" ]]; do sleep 1; done
	if [[ "$(<"$E2E_IMAGES_EXIT_FILE")" != 0 ]]; then
		echo "the e2e images did not build:" >&2
		cat "${E2E_IMAGES_LOG:-/dev/null}" >&2
		log "image build failed"
		exit 1
	fi
	log "images built"
}

# Without ci-kind-config, every other image a pod in the cluster pulls comes
# through the Docker daemon, so the node's containerd never pulls from a
# registry itself. The list is derived from the files that name the images, so
# Renovate keeps it current. In CI the nodes pull them through the caches.
preload=1
[[ -x "$ci_kind_config" ]] && preload=0
if [[ "$preload" == 1 ]]; then
	log "listing the images the cluster's pods pull"
	./scripts/ci/e2e-images.sh >"$work/images"
	log "pulling $(wc -l <"$work/images") images through the Docker daemon"
	pull_image() { docker pull --quiet "$1" >/dev/null; }
	export -f pull_image
	# shellcheck disable=SC2016 # $1 must expand in the child shell, not here
	xargs -P 8 -I{} bash -c 'pull_image "$1"' _ {} <"$work/images"
fi

if [[ -n "$cluster_pid" ]]; then
	log "waiting for the kind cluster"
	if ! wait "$cluster_pid"; then
		cluster_pid=""
		cat "$cluster_log" >&2
		log "kind cluster creation failed"
		exit 1
	fi
	cluster_pid=""
	cat "$cluster_log" >&2
fi
wait_for_images
log "loading the images into kind"
kind load docker-image "$server_image" "$client_image" --name "$cluster" >&2
if [[ "$preload" == 1 ]]; then
	# kind load fails on multi-platform images from the Docker daemon's containerd
	# store (it imports every platform, but the daemon holds only one). Import each
	# image with containerd's own import instead, named as a pod's pinned reference
	# names it: the repository, then the digest.
	node="${cluster}-control-plane"
	import_image() {
		local ref="$1"
		local repo="${ref%%[:@]*}"
		docker save "$ref" | docker exec -i --privileged "$node" ctr --namespace=k8s.io images import --digests --base-name "$repo" --snapshotter=overlayfs - >/dev/null
	}
	export -f import_image
	export node
	# shellcheck disable=SC2016 # $1 must expand in the child shell, not here
	xargs -P 4 -I{} bash -c 'import_image "$1"' _ {} <"$work/images" >&2
fi

cat >"$env_file" <<ENV
E2E_CLUSTER=${cluster}
E2E_KUBE_CONTEXT=kind-${cluster}
E2E_SERVER_IMAGE=${server_image}
E2E_CLIENT_IMAGE=${client_image}
ENV
log "ready: ${env_file}"
echo "$env_file"
