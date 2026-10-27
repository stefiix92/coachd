#!/usr/bin/env bash
# Runs the watch app's (:test) suite in the Connect IQ simulator on each
# device given (default fenix7spro), and fails unless every run passes.
#
#   ./simtest.sh fenix7spro fenix6pro
#
# Start the simulator first ("$CIQ_SDK/bin/connectiq"; the watch workflow runs
# the Linux one under Xvfb). Each device gets a fresh "build.sh test" (CIQ_SDK
# as there; COACHD_PUBLIC_URL defaults to https://coachd.example.com), then
# monkeydo. monkeydo exits 1 even when every test passes, so a run passes when
# its output ends with PASSED. Where the timeout command exists, a run that
# takes over SIMTEST_TIMEOUT seconds (default 300) fails: a wedged simulator
# can leave monkeydo waiting forever.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sdk="${CIQ_SDK:-$HOME/.cache/connectiq/sdk-9.3.0}"
prg="$here/bin/coachd-test.prg"
out="$here/bin/simtest.log"
export COACHD_PUBLIC_URL="${COACHD_PUBLIC_URL:-https://coachd.example.com}"

monkeydo() {
	if command -v timeout >/dev/null; then
		timeout -k 10 "${SIMTEST_TIMEOUT:-300}" "$sdk/bin/monkeydo" "$@"
	else
		"$sdk/bin/monkeydo" "$@"
	fi
}

[[ $# -gt 0 ]] || set -- fenix7spro
failed=()
for device in "$@"; do
	# A failed build leaves the previous prg behind, and monkeydo would run
	# that one: remove it first.
	rm -f "$prg"
	if ! CIQ_DEVICE="$device" "$here/build.sh" test; then
		failed+=("$device")
		continue
	fi
	monkeydo "$prg" "$device" -t >"$out" 2>&1 || true
	cat "$out"
	if [[ "$(grep -v '^[[:space:]]*$' "$out" | tail -n 1)" == PASSED* ]]; then
		echo "simtest.sh: $device passed" >&2
	else
		echo "simtest.sh: $device FAILED" >&2
		failed+=("$device")
	fi
done
[[ ${#failed[@]} -eq 0 ]] || {
	echo "simtest.sh: failed on ${failed[*]}" >&2
	exit 1
}
