#!/bin/sh
# Cleans dockerd's storage at the end of a master push run on Tekton, before
# the cache promoter may snapshot it, and fails when it is still nearly full.
# The builder's garbage collection limits only the build cache: old CI images
# and old versions of the preloaded e2e images would otherwise fill the volume until master runs
# fail and no new snapshot is promoted.
#
# It runs in a container without bash, so the caller lists the images to keep
# (scripts/ci/e2e-images.sh --node, which needs bash) and passes the file.
#
#   scripts/ci/cleanup.sh <current CI image tag> <file listing the e2e images to keep>
set -eu
current="${1:?usage: cleanup.sh <current CI image tag> <keep file>}"
keep="${2:?usage: cleanup.sh <current CI image tag> <keep file>}"
# The build cache budget: the CI image's layers (~3.5 GB) and the e2e image
# builds, with room for one flake change.
build_cache_budget="${CI_BUILD_CACHE_BUDGET:-8GB}"
max_percent=80

docker image prune -f >/dev/null
for old in $(docker image ls --format '{{.Repository}}:{{.Tag}}' githubgate-ci); do
	if [ "$old" != "$current" ]; then
		docker image rm "$old" >/dev/null
	fi
done
# Preloaded e2e images the tree no longer names (older versions after a bump).
[ -s "$keep" ] || { echo "cleanup: no e2e images listed, not removing any" >&2; exit 1; }
for stale in $(docker image ls --format '{{.Repository}}:{{.Tag}}' | /bin/sh "$(dirname "$0")/stale-images.sh" "$keep"); do
	docker image rm "$stale" >/dev/null
done
docker builder prune -f --keep-storage "$build_cache_budget" >/dev/null

# dockerd's storage is its own filesystem, mounted only in the dind sidecar.
used=$(docker run --rm -v /var/lib/docker:/storage:ro --entrypoint /bin/sh "$current" \
	-c 'df -P /storage' | /bin/sh "$(dirname "$0")/df-percent.sh")
docker system df
echo "docker storage: ${used}% used after cleanup"
if [ "$used" -gt "$max_percent" ]; then
	echo "docker storage is ${used}% full after cleanup" >&2
	exit 1
fi
