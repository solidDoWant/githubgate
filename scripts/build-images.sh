#!/usr/bin/env bash
# Builds the two images for linux/amd64 and linux/arm64 (`make build-images`):
#   - ghcr.io/soliddowant/githubgate:$VERSION from Dockerfile (the server);
#   - ghcr.io/soliddowant/githubgate-client:$VERSION from Dockerfile.client
#     (the client alone, FROM scratch, for image-volume mounts).
# Without PUSH_ALL=true the images for the host architecture are loaded into
# the local Docker daemon; PUSH_ALL=true pushes the multi-arch images
# (releases only).
# Each binary is scanned with govulncheck -mode=binary. Findings are reported
# but do not stop the build (VULNCHECK_STRICT=true makes them fail it): the
# release gate decides whether known vulnerabilities block a release.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version="${VERSION:-0.0.0-dev}"
ldflags="-s -w -X main.version=${version}"

# name of the binary (and of its cmd/ directory), image, Dockerfile.
images=(
	"githubgate ghcr.io/soliddowant/githubgate:${version} Dockerfile"
	"githubgate-client ghcr.io/soliddowant/githubgate-client:${version} Dockerfile.client"
)

for entry in "${images[@]}"; do
	read -r binary _ _ <<<"$entry"
	for arch in amd64 arm64; do
		mkdir -p "build/images/$arch"
		CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="$ldflags" -o "build/images/$arch/$binary" "./cmd/$binary"
		if ! govulncheck -mode=binary "build/images/$arch/$binary"; then
			echo "build-images: govulncheck reported vulnerabilities in the $arch $binary binary" >&2
			[[ "${VULNCHECK_STRICT:-false}" == "true" ]] && exit 1
		fi
	done
done

host=$(go env GOARCH)
for entry in "${images[@]}"; do
	read -r _ image dockerfile <<<"$entry"
	if [[ "${PUSH_ALL:-false}" == "true" ]]; then
		docker buildx build --platform linux/amd64,linux/arm64 --file "$dockerfile" --tag "$image" --push .
		continue
	fi

	# A multi-arch image cannot be loaded into the Docker daemon, so each
	# architecture is built on its own and the host's one gets the plain tag.
	for arch in amd64 arm64; do
		tag="${image}-${arch}"
		docker buildx build --platform "linux/$arch" --file "$dockerfile" --tag "$tag" --load .
		if [[ "$arch" == "$host" ]]; then
			docker tag "$tag" "$image"
		fi
	done
	echo "build-images: built $image (linux/amd64 and linux/arm64 built; $host loaded as $image)"
done
