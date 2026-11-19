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
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/githubgate" ./cmd/githubgate
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/githubgate-client" ./cmd/githubgate-client
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w" -o "build/images/${arch}/fakegithub" ./cmd/fakegithub
docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$server_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.client --tag "$client_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.fakegithub --tag "$fake_image" --load . >&2
echo "e2e-images: built" >&2
