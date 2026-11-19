#!/usr/bin/env bash
# Regenerates the committed generated code (`make generate`) with controller-gen:
#   - deepcopy methods for the API types in api/;
#   - CRD manifests into config/crd/bases (the chart reads them from there);
#   - the manager's ClusterRole (from the +kubebuilder:rbac markers) into config/rbac/role.yaml;
#   - the chart's CRD and ClusterRole templates (scripts/generate-chart.sh).
#   - the Tekton CI PipelineRuns in .tekton/ (scripts/ci/tekton-generate.sh).
#   - the permission catalogue in internal/permissions (scripts/gen-permissions; needs network).
# Generated code is committed and never edited by hand. Until api/ has types
# and the code has +kubebuilder:rbac markers, controller-gen writes nothing and
# the chart has no CRD or ClusterRole templates.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if [[ -d api ]]; then
	controller-gen object paths=./api/...
fi
controller-gen rbac:roleName=manager-role crd paths=./... \
	output:crd:artifacts:config=config/crd/bases \
	output:rbac:artifacts:config=config/rbac

# The chart's CRD and ClusterRole templates, copied from the files above.
./scripts/generate-chart.sh

# The generated reference pages under docs/admin/.
./scripts/docs/generate-reference.sh

# The Tekton CI PipelineRuns in .tekton/, from their template.
./scripts/ci/tekton-generate.sh

# The GitHub permission catalogue, from the pinned REST API description.
go run ./scripts/gen-permissions
