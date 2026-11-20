#!/usr/bin/env bash
# Builds the e2e images (the server and the client) for the host architecture on the local Docker daemon, with the tags
# test/e2e/up.sh loads into kind. Two callers may run it at the same time on a
# developer machine (the two e2e tracks) and share the build tree and the tags,
# so it holds a lock for the whole run. CI runs it once before the e2e stages start.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

tag="e2e"
server_image="ghcr.io/soliddowant/githubgate:${tag}"
client_image="ghcr.io/soliddowant/githubgate-client:${tag}"
fake_image="ghcr.io/soliddowant/githubgate-fakegithub:${tag}"
arch="$(go env GOARCH)"

mkdir -p build/e2e
# mkdir is atomic and flock is not in every image.
lock=build/e2e/images.lockdir
until mkdir "$lock" 2>/dev/null; do
	if [[ -n "$(find "$lock" -maxdepth 0 -mmin +15 2>/dev/null)" ]]; then rmdir "$lock" 2>/dev/null || true; fi
	sleep 2
done
trap 'rmdir "$lock" 2>/dev/null || true' EXIT

echo "e2e-images: building the images for linux/${arch}" >&2
mkdir -p "build/images/${arch}"
# timed runs a command and prints how long it took, on stderr with the rest of the log.
timed() {
	local label="$1" begin=$SECONDS
	shift
	"$@" >&2
	echo "e2e-images: ${label} took $((SECONDS - begin))s" >&2
}
go_build() {
	local flags="$1" out="$2" pkg="$3"
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="$flags" -o "build/images/${arch}/${out}" "$pkg"
}
# Each image needs only its own binary, so build the three binary-and-image
# pipelines at the same time. Output goes to one log per pipeline and is printed
# in order afterwards, so the log stays readable.
build_one() {
	local name="$1" flags="$2" pkg="$3" dockerfile="$4" image="$5"
	timed "go build ${name}" go_build "$flags" "$name" "$pkg"
	timed "docker buildx build ${name}" docker buildx build --platform "linux/${arch}" --file "$dockerfile" --tag "$image" --load .
}
logs="$(mktemp -d)"
trap 'rmdir "$lock" 2>/dev/null || true; rm -rf "$logs"' EXIT
pids=()
names=(githubgate githubgate-client fakegithub)
build_one githubgate "-s -w -X main.version=${tag}" ./cmd/githubgate Dockerfile "$server_image" >"$logs/githubgate" 2>&1 &
pids+=($!)
build_one githubgate-client "-s -w -X main.version=${tag}" ./cmd/githubgate-client Dockerfile.client "$client_image" >"$logs/githubgate-client" 2>&1 &
pids+=($!)
build_one fakegithub "-s -w" ./cmd/fakegithub Dockerfile.fakegithub "$fake_image" >"$logs/fakegithub" 2>&1 &
pids+=($!)
status=0
for i in "${!pids[@]}"; do
	if ! wait "${pids[$i]}"; then status=1; fi
	cat "$logs/${names[$i]}" >&2
done
[[ "$status" == 0 ]] || { echo "e2e-images: a build failed" >&2; exit 1; }
echo "e2e-images: built in ${SECONDS}s" >&2
