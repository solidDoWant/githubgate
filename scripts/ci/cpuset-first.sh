#!/bin/sh
# Prints the first COUNT CPUs of a Linux cpuset list (such as
# cpuset.cpus.effective, "0-3,8-11") in the same list format, for the
# --cpuset-cpus option of Docker, then how many CPUs that list has: fewer than
# COUNT when the cpuset is smaller.
#
#   scripts/ci/cpuset-first.sh LIST COUNT
set -eu
list="${1:?usage: cpuset-first.sh LIST COUNT}"
count="${2:?usage: cpuset-first.sh LIST COUNT}"
echo "$list" | awk -v want="$count" -F, '
	{
		n = 0
		out = ""
		for (i = 1; i <= NF && n < want; i++) {
			if ($i !~ /^[0-9]+(-[0-9]+)?$/) { bad = 1; exit }
			split($i, r, "-")
			last = (r[2] == "") ? r[1] : r[2]
			for (c = r[1] + 0; c <= last + 0 && n < want; c++) {
				out = (n == 0) ? c : out "," c
				n++
			}
		}
	}
	END {
		if (bad || n == 0) { print "not a cpuset list: " $0 > "/dev/stderr"; exit 1 }
		print out, n
	}
'
