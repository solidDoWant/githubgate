#!/usr/bin/env bash
# Prepares the e2e environment on the kind cluster E2E_CLUSTER (default
# githubgate-e2e, created with test/e2e/kind-config.yaml, ImageVolume on): the
# server, client and fake GitHub images built for the host architecture and loaded into
# kind, and cert-manager installed. The tests install the fake GitHub and the chart themselves, because the
# chart needs its Secrets first. Prints the env file path; it holds the kube
# context and the image names.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

cluster="${E2E_CLUSTER:-githubgate-e2e}"
node_image="mirror.gcr.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
tag="e2e"
server_image="ghcr.io/soliddowant/githubgate:${tag}"
client_image="ghcr.io/soliddowant/githubgate-client:${tag}"
fake_image="ghcr.io/soliddowant/githubgate-fakegithub:${tag}"
env_file="build/e2e/${cluster}.env"
mkdir -p build/e2e

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

start=$SECONDS
start_time=$(date +%s)
log() { echo "e2e-up: [+$((SECONDS - start))s] $*" >&2; }

# The cluster needs nothing from the image builds and the builds need nothing
# from the cluster, so create it in the background while the images build. Only
# the image load needs both.
# Prefixes each line with the seconds since the script started, so the log
# shows where kind's own steps spend their time.
timestamp_lines() {
	while IFS= read -r line; do echo "[+$((SECONDS - start))s] $line"; done
}
ci_kind_config=/run/ci-kind/bin/ci-kind-config
# A node whose systemd never boots makes kind wait far longer than a healthy
# create takes (about a minute), so bound it.
create_timeout="${E2E_KIND_CREATE_TIMEOUT:-300}"

# Prints what is needed to tell why a node did not boot. The node container is
# kept (--retain) so its systemd output is still there. Each command may fail
# on its own without hiding the rest.
diagnose_node() {
	local node
	echo "==== kind node diagnostics for $cluster ===="
	echo "---- docker info"
	docker info 2>&1 || true
	echo "---- docker system df"
	docker system df 2>&1 || true
	echo "---- host load and memory"
	cat /proc/loadavg 2>&1 || true
	grep -E "MemTotal|MemAvailable" /proc/meminfo 2>&1 || true
	nproc 2>&1 || true
	echo "---- containers running in the daemon (other stages share it)"
	docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}' 2>&1 || true
	echo "---- dmesg (OOM kills, if readable)"
	{ dmesg 2>&1 || true; } | grep -i -E 'oom|out of memory|killed process|cgroup' | tail -40 || true
	echo "---- daemon events for the cluster's containers since the script started"
	timeout 20 docker events --since "$start_time" --until "$(date +%s)" --filter "label=io.x-k8s.kind.cluster=$cluster" 2>&1 || true
	for node in $(docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster" --format '{{.Names}}'); do
		echo "---- docker inspect state of $node"
		docker inspect --format 'status={{.State.Status}} exitcode={{.State.ExitCode}} oomkilled={{.State.OOMKilled}} restartcount={{.RestartCount}} error={{.State.Error}} restartpolicy={{.HostConfig.RestartPolicy.Name}}:{{.HostConfig.RestartPolicy.MaximumRetryCount}} created={{.Created}} startedat={{.State.StartedAt}} finishedat={{.State.FinishedAt}}' "$node" 2>&1 || true
		# Timestamps tell the first boot from the restarted one: a second
		# "Welcome to" banner starts the second boot.
		echo "---- docker logs $node (systemd output, all boots, timestamped)"
		docker logs --timestamps --tail 600 "$node" 2>&1 || true
		echo "---- limits of $node"
		docker inspect --format 'memory={{.HostConfig.Memory}} memswap={{.HostConfig.MemorySwap}} nanocpus={{.HostConfig.NanoCpus}} cpushares={{.HostConfig.CpuShares}} cgroupns={{.HostConfig.CgroupnsMode}} privileged={{.HostConfig.Privileged}} state={{.State.Status}} oom={{.State.OOMKilled}}' "$node" 2>&1 || true
		echo "---- cgroup of $node"
		docker exec "$node" sh -c 'cat /proc/self/cgroup; stat -fc %T /sys/fs/cgroup; cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/cpu.max 2>&1' 2>&1 || true
		docker exec "$node" systemctl list-jobs --no-pager 2>&1 | head -40 || true
	done
	echo "---- docker ps -a"
	docker ps -a 2>&1 || true
}
cluster_pid=""
cluster_log="$work/cluster.log"
# One attempt at creating the cluster.
create_cluster_once() {
	if [[ -x "$ci_kind_config" ]]; then
		# ci-kind-config keeps each node's image store between builds, keyed by
		# the cluster name, so it takes the name and image instead of kind.
		# Run it to completion first, so the log shows how long it takes apart
		# from kind, which reads its config to the end before it starts.
		echo "e2e-up: ci-kind-config start"
		"$ci_kind_config" --name "$cluster" --image "$node_image" test/e2e/kind-config.yaml >"$work/kind-config.yaml"
		echo "e2e-up: ci-kind-config end, kind start"
		timeout --kill-after=10 "$create_timeout" kind create cluster -v 1 --retain --config="$work/kind-config.yaml" --wait 120s
	else
		timeout --kill-after=10 "$create_timeout" kind create cluster -v 1 --retain --name "$cluster" --image "$node_image" --config test/e2e/kind-config.yaml --wait 120s
	fi
}
# kind gives the node only 30 seconds to reach the multi-user target. On a busy
# machine containerd alone can take longer than that to start, although the node
# does boot a little later. Starting over finds the node image and its cache
# warm, so retry a create that failed only for that reason. Any other failure,
# and a second failure, is final.
create_cluster() {
	local attempt out
	for attempt in 1 2; do
		out="$work/create-$attempt.log"
		# tee passes each line on as it arrives, so the caller's timestamps
		# show when each kind step ran; the copy in $out is searched below.
		if create_cluster_once 2>&1 | tee "$out"; then
			echo "e2e-up: kind end"
			return 0
		fi
		echo "e2e-up: kind end (failed)"
		if [[ "$attempt" == 2 ]] || ! grep -q 'Multi-User System' "$out"; then
			return 1
		fi
		echo "kind node did not reach the multi-user target in time; trying again"
		# Keep the first node's boot output, which is what explains the delay.
		for node in $(docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster" --format '{{.Names}}'); do
			echo "---- first attempt: docker logs $node (last 80 lines, timestamped)"
			docker logs --timestamps --tail 80 "$node" 2>&1 | sed 's/\x1b\[[0-9;?]*[A-Za-z]//g' || true
		done
		kind delete cluster --name "$cluster" || true
	done
}
if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	log "creating kind cluster $cluster (in the background)"
	create_cluster 2>&1 | timestamp_lines >"$cluster_log" &
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
	# Print the build log on success too, so its step timings are visible.
	cat "${E2E_IMAGES_LOG:-/dev/null}" >&2
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
		diagnose_node >&2
		kind delete cluster --name "$cluster" >&2 || true
		log "kind cluster creation failed"
		exit 1
	fi
	cluster_pid=""
	cat "$cluster_log" >&2
fi

# cert-manager needs none of our images, so install it in the background as
# soon as the cluster is ready, while the images build. Without ci-kind-config
# the nodes get their images by import, so that case installs it after the
# import below.
cert_manager_pid=""
cert_manager_log="$work/cert-manager.log"
install_cert_manager() {
	log "installing cert-manager (in the background)"
	E2E_KUBE_CONTEXT="kind-${cluster}" ./test/e2e/install-cert-manager.sh >"$cert_manager_log" 2>&1 &
	cert_manager_pid=$!
}
if [[ "$preload" == 0 ]]; then install_cert_manager; fi
trap 'stop_cluster; if [[ -n "$cert_manager_pid" ]]; then kill "$cert_manager_pid" 2>/dev/null || true; fi; rm -rf "$work"' EXIT

wait_for_images
log "loading the images into kind"
kind load docker-image "$server_image" "$client_image" "$fake_image" --name "$cluster" >&2
log "images loaded into kind"
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

# The injection webhook names the client image by digest, and kind loads it by
# tag only, so add the digest reference to the node's image store.
node="${cluster}-control-plane"
client_digest="$(docker exec "$node" ctr --namespace=k8s.io images ls "name==${client_image}" | awk -v ref="$client_image" '$1 == ref { print $3 }')"
if [[ ! "$client_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
	echo "e2e-up: the client image has no digest on the node: '${client_digest}'" >&2
	exit 1
fi
docker exec "$node" ctr --namespace=k8s.io images tag --force "$client_image" "${client_image%%:*}@${client_digest}" >/dev/null

if [[ "$preload" == 1 ]]; then install_cert_manager; fi
log "waiting for cert-manager"
if ! wait "$cert_manager_pid"; then
	cert_manager_pid=""
	cat "$cert_manager_log" >&2
	log "cert-manager install failed"
	exit 1
fi
cert_manager_pid=""
cat "$cert_manager_log" >&2
log "cert-manager ready"

cat >"$env_file" <<ENV
E2E_CLUSTER=${cluster}
E2E_KUBE_CONTEXT=kind-${cluster}
E2E_SERVER_IMAGE=${server_image}
E2E_CLIENT_IMAGE=${client_image}
E2E_CLIENT_IMAGE_DIGEST=${client_digest}
E2E_FAKE_IMAGE=${fake_image}
ENV
log "ready: ${env_file}"
echo "$env_file"
