#!/usr/bin/env bash
# Prints the fēnix and Forerunner models the watch app supports, one Connect IQ
# device id per line, from the device files the Connect IQ SDK Manager
# installed. A model qualifies when it runs watch apps at API level 3.0 or
# later and has app workouts (PersistedContent.getAppWorkouts) and FIT
# downloads (Communications.HTTP_RESPONSE_CONTENT_TYPE_FIT), which the sync
# needs. Preview device files are skipped. Paste the result into the
# <iq:products> of manifest.xml.
set -euo pipefail

devices="${CIQ_DEVICES:-$HOME/Library/Application Support/Garmin/ConnectIQ/Devices}"
[[ -d "$devices" ]] || {
	echo "devices.sh: no device files at $devices" >&2
	exit 2
}

for dir in "$devices"/fenix* "$devices"/fr*; do
	[[ -d "$dir" ]] || continue
	id="$(basename "$dir")"
	[[ "$id" != *preview* ]] || continue
	api="$dir/$id.api.debug.xml"
	[[ -f "$dir/compiler.json" && -f "$api" ]] || continue
	if ! grep -q getAppWorkouts "$api" || ! grep -q HTTP_RESPONSE_CONTENT_TYPE_FIT "$api"; then
		continue
	fi
	python3 -I - "$dir/compiler.json" <<'PY' || continue
import json, sys
c = json.load(open(sys.argv[1]))
watch_app = any(a.get("type") == "watchApp" for a in c.get("appTypes", []))
def version(v):
    return tuple(int(x) for x in v.split(".")[:3])
api = max((version(p["connectIQVersion"]) for p in c.get("partNumbers", []) if p.get("connectIQVersion")), default=(0,))
sys.exit(0 if watch_app and api >= (3, 0, 0) else 1)
PY
	echo "$id"
done | sort
