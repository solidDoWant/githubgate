#!/usr/bin/env bash
# Checks that up.sh passes each image list entry to its pull and import
# functions as data, so a hostile entry never runs as shell code.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf '%s\n' \
	'quay.io/example/ok:v1' \
	"quay.io/example/bad:v1\$(touch \${IFS}$work/pwned-subst)" \
	"quay.io/example/bad:v2;touch \${IFS}$work/pwned-semi" \
	"quay.io/example/bad:v3\`touch \${IFS}$work/pwned-tick\`" >"$work/images"

# Stand-ins for the functions up.sh defines: record each argument.
pull_image() { printf '%s\n' "$1" >>"$work/pulled"; }
import_image() { printf '%s\n' "$1" >>"$work/imported"; }
export -f pull_image import_image
export work

# Run the xargs lines of up.sh exactly as written.
grep -E '^[[:space:]]*xargs ' "$here/up.sh" | sed -E "s|\"\$work/images\"|\"$work/images\"|" >"$work/loops.sh"
[ "$(wc -l <"$work/loops.sh")" -eq 2 ] || { echo "expected two xargs loops in up.sh" >&2; exit 1; }
bash "$work/loops.sh" >/dev/null

if [ -n "$(find "$work" -name "pwned-*")" ]; then
	echo "FAIL: an image list entry ran as shell code" >&2
	exit 1
fi
for f in pulled imported; do
	[ "$(sort "$work/$f")" = "$(sort "$work/images")" ] || { echo "FAIL: $f differs from the list" >&2; exit 1; }
done
echo "ok"
