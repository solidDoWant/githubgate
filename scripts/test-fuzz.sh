#!/usr/bin/env bash
# Runs every fuzz target at the same time, each for FUZZTIME with at most
# FUZZ_PARALLEL workers, so fuzzing does not starve the other CI stages.
#
#   scripts/test-fuzz.sh package:FuzzName...
#
# Go's fuzz coordinator can time out on a stalled worker under CPU contention
# ("context deadline exceeded") without finding a crash. A target that fails
# that way and saved no failing input is run once more; any other failure
# fails at once.
set -u

fuzztime="${FUZZTIME:-30s}"
workers="${FUZZ_PARALLEL:-4}"
logdir=$(mktemp -d)
trap 'rm -rf "$logdir"' EXIT

run_target() {
	local target="$1" log="$2"
	go test -run '^$' -fuzz "^${target#*:}\$" -fuzztime "$fuzztime" -parallel "$workers" "${target%%:*}" >"$log" 2>&1
}

fuzz_one() {
	local target="$1" log="$2"
	echo "fuzzing $target for $fuzztime with $workers workers"
	if run_target "$target" "$log"; then
		return 0
	fi
	if grep -q 'context deadline exceeded' "$log" && ! grep -q 'Failing input written' "$log"; then
		echo "$target: the fuzz coordinator timed out and found no failing input; running it once more"
		run_target "$target" "$log.retry"
		local status=$?
		mv "$log.retry" "$log"
		return "$status"
	fi
	return 1
}

pids=()
targets=("$@")
for index in "${!targets[@]}"; do
	fuzz_one "${targets[$index]}" "$logdir/$index.log" &
	pids+=($!)
done

failed=0
for index in "${!pids[@]}"; do
	status=0
	wait "${pids[$index]}" || status=$?
	echo "==================== ${targets[$index]}: exit $status ===================="
	cat "$logdir/$index.log"
	if [ "$status" != 0 ]; then failed=1; fi
done
exit "$failed"
