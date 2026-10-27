#!/usr/bin/env bash
# Fetches what the watch app's CI builds need on Linux (.github/workflows/
# watch.yml and release.yml). Everything comes from Garmin at build time:
# nothing of Garmin's is kept in this repository.
#
#   CIQ_SDK=/path ./ci-setup.sh sdk     # the Linux Connect IQ SDK, unpacked into $CIQ_SDK
#   ./ci-setup.sh devices [device...]   # the device files of every product in manifest.xml,
#                                       # plus the simulator fonts of the devices given
#
# sdk downloads the pinned SDK from developer.garmin.com (no login) and checks
# its SHA-256.
#
# devices needs a Garmin account (GARMIN_USERNAME and GARMIN_PASSWORD, without
# two-step verification), as the Connect IQ SDK Manager does. It uses
# lindell/connect-iq-sdk-manager-cli (pinned, SHA-256 checked) to accept the
# Connect IQ SDK License Agreement, log in and download into
# ~/.Garmin/ConnectIQ, where the Linux monkeyc and simulator read them. The
# agreement is accepted only while Garmin's text matches agreement_hash, the
# version the owner accepted: when Garmin changes it, this fails until the
# owner reads the new one and updates the hash. The login token is deleted on
# exit. Keep the CLI's default log level: at trace it logs request bodies,
# the password included.
#
# Linux x86-64 only (the Linux simulator is x86-64; sha256sum).
set -euo pipefail

sdk_file="connectiq-sdk-lin-9.3.0-2026-10-20-95888f262.zip"
sdk_sha256="1653cc98edd70029670a3e3fcd4ad6e746974a8b758a7b736a4e11506dfd8bc8"
cli_version="0.8.4"
cli_sha256="5d57b8f9cfcd02d3b3f48e19d6daca0480e4c6f627926f15d8617ef8a6f57e0b"
agreement_hash="CC737BBBA104D7740D5BBCF27D0F6382"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() {
	echo "ci-setup.sh: $*" >&2
	exit 2
}

# fetch URL FILE SHA256: download and check.
fetch() {
	curl -fsSL --retry 3 -o "$2" "$1"
	echo "$3  $2" | sha256sum -c --quiet - || die "SHA-256 mismatch for $1"
}

tmp="$(mktemp -d)"
cleanup() {
	rm -rf "$tmp"
	rm -f "$HOME/.Garmin/ConnectIQ/token.json"
}
trap cleanup EXIT

case "${1:-}" in
sdk)
	[[ $# -eq 1 ]] || die "usage: ci-setup.sh sdk"
	[[ -n "${CIQ_SDK:-}" ]] || die "set CIQ_SDK to the directory to unpack the SDK into"
	fetch "https://developer.garmin.com/downloads/connect-iq/sdks/$sdk_file" "$tmp/sdk.zip" "$sdk_sha256"
	mkdir -p "$CIQ_SDK"
	unzip -q -o "$tmp/sdk.zip" -d "$CIQ_SDK"
	echo "ci-setup.sh: Connect IQ SDK $(cat "$CIQ_SDK/bin/version.txt") in $CIQ_SDK" >&2
	;;
devices)
	shift
	[[ -n "${GARMIN_USERNAME:-}" && -n "${GARMIN_PASSWORD:-}" ]] ||
		die "set GARMIN_USERNAME and GARMIN_PASSWORD (a Garmin account) to download device files"
	fetch "https://github.com/lindell/connect-iq-sdk-manager-cli/releases/download/v$cli_version/connect-iq-sdk-manager-cli_${cli_version}_Linux_x86_64.tar.gz" \
		"$tmp/cli.tar.gz" "$cli_sha256"
	tar -xzf "$tmp/cli.tar.gz" -C "$tmp" connect-iq-sdk-manager
	cli="$tmp/connect-iq-sdk-manager"
	"$cli" agreement accept --agreement-hash "$agreement_hash"
	"$cli" login # reads GARMIN_USERNAME and GARMIN_PASSWORD from the environment
	"$cli" device download --manifest "$here/manifest.xml"
	if [[ $# -gt 0 ]]; then
		fonts=()
		for d in "$@"; do fonts+=(--device "$d"); done
		"$cli" device download "${fonts[@]}" --include-fonts
	fi
	;;
*) die "usage: ci-setup.sh sdk | devices [device...]" ;;
esac
