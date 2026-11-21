#!/bin/sh
# Picks COUNT CPUs of a Linux cpuset list (such as cpuset.cpus.effective,
# "0-3,8-11") for the --cpuset-cpus option of Docker. Prints the chosen list,
# then how many CPUs that list has (fewer than COUNT when the cpuset is
# smaller).
#
# With two samples of /proc/stat taken a moment apart, it picks the run of COUNT
# neighbouring CPUs of the cpuset that were busiest the least between the
# samples, so runs on one node don't pin to the same CPUs. It then prints a
# second line with the busy ticks of each CPU of the cpuset. Without samples it
# takes the first COUNT CPUs.
#
#   scripts/ci/cpuset-first.sh LIST COUNT [STAT_BEFORE STAT_AFTER]
set -eu
list="${1:?usage: cpuset-first.sh LIST COUNT [STAT_BEFORE STAT_AFTER]}"
count="${2:?usage: cpuset-first.sh LIST COUNT [STAT_BEFORE STAT_AFTER]}"
before="${3:-/dev/null}"
after="${4:-/dev/null}"
# The samples come first, so awk can tell them from the list by file number.
awk -v list="$list" -v want="$count" '
	# Busy ticks per CPU: everything but idle and iowait (fields 5 and 6).
	/^cpu[0-9]+ / {
		cpu = substr($1, 4) + 0
		total = 0
		for (i = 2; i <= NF; i++) total += $i
		busy = total - $5 - $6
		if (FILENAME == ARGV[1]) first[cpu] = busy
		else { delta[cpu] = busy - first[cpu]; sampled = 1 }
	}
	END {
		n = split(list, parts, ",")
		m = 0
		for (i = 1; i <= n; i++) {
			if (parts[i] !~ /^[0-9]+(-[0-9]+)?$/) { bad = 1; break }
			split(parts[i], r, "-")
			last = (r[2] == "") ? r[1] : r[2]
			for (c = r[1] + 0; c <= last + 0; c++) cpus[++m] = c
		}
		if (bad || m == 0) { print "not a cpuset list: " list > "/dev/stderr"; exit 1 }
		take = (m < want + 0) ? m : want + 0
		sum = 0
		for (i = 1; i <= take; i++) sum += delta[cpus[i]] + 0
		best = sum; start = 1
		for (s = 2; s + take - 1 <= m; s++) {
			sum += delta[cpus[s + take - 1]] - delta[cpus[s - 1]]
			if (sum < best) { best = sum; start = s }
		}
		out = ""
		for (i = start; i < start + take; i++) out = (i == start) ? cpus[i] : out "," cpus[i]
		print out, take
		if (sampled) {
			info = ""
			for (i = 1; i <= m; i++) info = info (i > 1 ? "," : "") cpus[i] "=" (delta[cpus[i]] + 0)
			print info
		}
	}
' "$before" "$after"
