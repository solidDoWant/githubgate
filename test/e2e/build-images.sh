#!/usr/bin/env bash
# Builds the e2e images (the server and the client) for the host architecture on the local Docker daemon, with the tags
# test/e2e/up.sh loads into kind. Two callers may run it at the same time on a
# developer machine (the two e2e tracks) and share the build tree and the tags,
# so it builds one at a time. CI runs it once before the e2e stages start.
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
timed "go build githubgate" go_build "-s -w -X main.version=${tag}" githubgate ./cmd/githubgate
timed "go build githubgate-client" go_build "-s -w -X main.version=${tag}" githubgate-client ./cmd/githubgate-client
timed "go build fakegithub" go_build "-s -w" fakegithub ./cmd/fakegithub
timed "docker buildx build githubgate" docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$server_image" --load .
timed "docker buildx build githubgate-client" docker buildx build --platform "linux/${arch}" --file Dockerfile.client --tag "$client_image" --load .
timed "docker buildx build fakegithub" docker buildx build --platform "linux/${arch}" --file Dockerfile.fakegithub --tag "$fake_image" --load .
echo "e2e-images: built in ${SECONDS}s" >&2
