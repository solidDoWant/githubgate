#!/bin/sh
# Tests scripts/ci/cpuset-first.sh: the first N CPUs of a cpuset list, and how
# many that is when the list has fewer.
set -eu
cd "$(dirname "$0")"
failed=0
check() {
	list="$1" want_count="$2" want="$3"
	got=$(./cpuset-first.sh "$list" "$want_count")
	if [ "$got" != "$want" ]; then
		echo "cpuset-first.sh '$list' $want_count printed '$got', want '$want'" >&2
		failed=1
	fi
}
check 0-31 8 '0,1,2,3,4,5,6,7 8'
check 0-7 8 '0,1,2,3,4,5,6,7 8'
check 0-6 8 '0,1,2,3,4,5,6 7'
check 0 8 '0 1'
check 0-1,4-9 5 '0,1,4,5,6 5'
check 2,5,7-30 3 '2,5,7 3'
check 0-31 1 '0 1'
if ./cpuset-first.sh '' 8 >/dev/null 2>&1; then
	echo "cpuset-first.sh accepted an empty cpuset list" >&2
	failed=1
fi
if ./cpuset-first.sh 'x-y' 8 >/dev/null 2>&1; then
	echo "cpuset-first.sh accepted a malformed cpuset list" >&2
	failed=1
fi
[ "$failed" = 0 ] || exit 1
echo "cpuset-first_test.sh: ok"
