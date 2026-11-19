#!/usr/bin/env bash
# Publishes the images and the chart of a release: the release stage of the
# Tekton release run (scripts/ci/stages.sh release). Runs in the CI image,
# where docker, crane and helm reach ghcr.io through the CI cluster's
# registrygate (the owner's registry proxy), which pushes only for the release
# ServiceAccount. The GitHub release is a separate
# task, so this script never holds a GitHub token.
#
#   scripts/ci/publish-release.sh <version>|v<version>
#
# In order, each step safe to run again for the same version:
#   1. both images for linux/amd64 and linux/arm64 (`make build-images`);
#   2. for a final version (no -rc.K suffix), both images tagged latest;
#   3. the chart, pushed to oci://ghcr.io/soliddowant/charts.
# PUBLISH_DRY_RUN=true prints the commands instead of running them (tests).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version="${1:?usage: publish-release.sh <version>}"
version="${version#v}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
	echo "publish-release: '$version' is not a release version (X.Y.Z or X.Y.Z-rc.K)" >&2
	exit 1
fi

run() {
	if [[ "${PUBLISH_DRY_RUN:-false}" == "true" ]]; then
		echo "would run: $*"
	else
		"$@"
	fi
}

images=(ghcr.io/soliddowant/githubgate ghcr.io/soliddowant/githubgate-client)

run make build-images VERSION="$version" PUSH_ALL=true
if [[ "$version" != *-* ]]; then
	for image in "${images[@]}"; do
		run crane tag "$image:$version" latest
	done
fi
run helm package deploy/charts/githubgate --version "$version" --app-version "$version" --destination build/chart
run helm push "build/chart/githubgate-$version.tgz" oci://ghcr.io/soliddowant/charts
echo "publish-release: published $version"
