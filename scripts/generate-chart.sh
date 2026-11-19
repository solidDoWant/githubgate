#!/usr/bin/env bash
# Writes the chart's generated templates from the files controller-gen produced
# (run by `make generate`; never edit the outputs by hand):
#   - templates/crds/*.yaml from config/crd/bases, each behind the crds.install value;
#   - templates/clusterrole.yaml from the rules in config/rbac/role.yaml.
# Either is absent while controller-gen has nothing to generate it from.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart=deploy/charts/githubgate
rm -rf "$chart/templates/crds" "$chart/templates/clusterrole.yaml"
shopt -s nullglob
crds=(config/crd/bases/*.yaml)
if ((${#crds[@]} > 0)); then mkdir -p "$chart/templates/crds"; fi
for crd in "${crds[@]}"; do
	out="$chart/templates/crds/$(basename "$crd")"
	{
		echo '{{- if .Values.crds.install }}'
		cat "$crd"
		echo '{{- end }}'
	} >"$out"
done

if [[ ! -f config/rbac/role.yaml ]]; then
	exit 0
fi
{
	echo '# Generated from config/rbac/role.yaml by make generate; do not edit.'
	cat <<'HEADER'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "githubgate.fullname" . }}
  labels:
    {{- include "githubgate.labels" . | nindent 4 }}
HEADER
	sed -n '/^rules:/,$p' config/rbac/role.yaml
} >"$chart/templates/clusterrole.yaml"
