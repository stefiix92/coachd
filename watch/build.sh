#!/usr/bin/env bash
# Builds the coachd Connect IQ watch app.
#
#   COACHD_PUBLIC_URL=https://coachd.example.com ./build.sh        # bin/coachd.prg, sideload
#   COACHD_PUBLIC_URL=https://coachd.example.com ./build.sh test   # bin/coachd-test.prg, with the (:test) suite
#   ./build.sh store                                               # bin/coachd.iq, the public Store app
#   ./build.sh store-beta                                          # bin/coachd-beta.iq, the Store beta
#
# Two app ids:
#  - public (manifest.xml, "coachd"): only the store package;
#  - dev (8643d463..., "coachd dev"): sideload, test and store-beta builds, so
#    a sideloaded or beta app sits next to the Store install instead of
#    replacing it. A Store beta cannot use the public app's id.
# The dev id goes into bin/manifest-dev.xml and bin/dev.jungle, generated on
# every build; tracked files are never edited. A jungle cannot reset
# project.manifest, so bin/dev.jungle replaces monkey.jungle for those builds
# (test builds add test/ to its source path): keep the two in step.
#
# The coachd URL is a setting in the Connect IQ phone app. Sideloaded apps get
# no phone settings, so sideload and test builds compile COACHD_PUBLIC_URL
# (https, required) into source/Config.mc as the setting's default. Store
# builds take no URL and embed none: every runner sets their own.
#
# CIQ_SDK points at the Connect IQ SDK (default ~/.cache/connectiq/sdk-9.3.0).
# CIQ_DEVICE picks the device of a sideload or test build: any product in
# manifest.xml, default fenix7spro. A test build falls back to fenix6pro when
# the fenix7spro device file is missing. Store builds target every product
# and need all their device files (Connect IQ SDK Manager). monkeyc reads them
# from ~/Library/Application Support/Garmin/ConnectIQ/Devices on macOS and
# ~/.Garmin/ConnectIQ/Devices on Linux.
# The developer key is <repo>/.secrets/ciq-developer.der; it is created on the
# first build and signs every later build, Store uploads included, so keep it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"
sdk="${CIQ_SDK:-$HOME/.cache/connectiq/sdk-9.3.0}"
key="$root/.secrets/ciq-developer.der"
if [[ "$(uname)" == Darwin ]]; then
	devices="$HOME/Library/Application Support/Garmin/ConnectIQ/Devices"
else
	devices="$HOME/.Garmin/ConnectIQ/Devices"
fi
dev_id="8643d4638c924746b3031c79d74c19bb"

die() {
	echo "build.sh: $*" >&2
	exit 2
}

usage="usage: build.sh [test|store|store-beta]"
case "${1:-}" in
"") mode="release" ;;
test | store | store-beta) mode="$1" ;;
*) die "$usage" ;;
esac
[[ $# -le 1 ]] || die "$usage"

if [[ $mode == store || $mode == store-beta ]]; then
	[[ -z "${COACHD_PUBLIC_URL:-}" ]] || die "store builds take no URL: unset COACHD_PUBLIC_URL"
	url=""
else
	# The URL ends up inside a Monkey C string literal: allow only plain URL
	# characters, and drop trailing slashes (paths are appended as "/device/v1/...").
	url="${COACHD_PUBLIC_URL:-}"
	while [[ "$url" == */ ]]; do url="${url%/}"; done
	[[ "$url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$ ]] ||
		die "COACHD_PUBLIC_URL must be an https URL such as https://coachd.example.com"
fi

[[ -x "$sdk/bin/monkeyc" ]] || die "no Connect IQ SDK at $sdk (set CIQ_SDK)"

products=()
while IFS= read -r p; do products+=("$p"); done < <(sed -n 's/.*<iq:product id="\([^"]*\)".*/\1/p' "$here/manifest.xml")
[[ ${#products[@]} -gt 0 ]] || die "no products in manifest.xml"

if [[ $mode == store || $mode == store-beta ]]; then
	missing=()
	for p in "${products[@]}"; do
		[[ -d "$devices/$p" ]] || missing+=("$p")
	done
	[[ ${#missing[@]} -eq 0 ]] || die "device files missing (install them with the Connect IQ SDK Manager): ${missing[*]}"
else
	device="${CIQ_DEVICE:-}"
	if [[ -n "$device" ]]; then
		[[ " ${products[*]} " == *" $device "* ]] || die "CIQ_DEVICE must be a product in manifest.xml: ${products[*]}"
	elif [[ -d "$devices/fenix7spro" ]]; then
		device=fenix7spro
	elif [[ $mode == test ]]; then
		device=fenix6pro
	fi
	if [[ "$device" == "" ]]; then
		die "fenix7spro device file missing: install it with the Connect IQ SDK Manager (or set CIQ_DEVICE)"
	fi
	[[ -d "$devices/$device" ]] || die "$device device file missing: install it with the Connect IQ SDK Manager"
fi

if [[ ! -f "$key" ]]; then
	[[ -d "$root/.secrets" ]] || mkdir -m 700 "$root/.secrets"
	# RSA 4096 as unencrypted PKCS#8 DER, the format monkeyc -y reads. The key
	# only ever goes to the file (mode 0600), never to the terminal.
	(
		umask 077
		trap 'rm -f "$key.tmp"' EXIT
		openssl genrsa 4096 | openssl pkcs8 -topk8 -outform DER -nocrypt -out "$key.tmp"
		chmod 600 "$key.tmp"
		mv "$key.tmp" "$key"
	)
	echo "build.sh: created developer key $key (keep it: it signs every update of the app)" >&2
fi

sed "s#\"https://coachd.example.com\"#\"$url\"#" "$here/source/Config.mc.example" >"$here/source/Config.mc"
grep -qF "DEFAULT_URL as String = \"$url\";" "$here/source/Config.mc" || die "could not write source/Config.mc"

mkdir -p "$here/bin"
if [[ $mode == store ]]; then
	jungles="$here/monkey.jungle"
else
	public_id="$(sed -n 's/.*<iq:application id="\([0-9a-f]*\)".*/\1/p' "$here/manifest.xml")"
	[[ -n "$public_id" ]] || die "no application id in manifest.xml"
	sed -e "s/id=\"$public_id\"/id=\"$dev_id\"/" -e 's/name="@Strings.AppName"/name="@Strings.AppNameDev"/' \
		"$here/manifest.xml" >"$here/bin/manifest-dev.xml"
	grep -qF "id=\"$dev_id\"" "$here/bin/manifest-dev.xml" &&
		grep -qF 'name="@Strings.AppNameDev"' "$here/bin/manifest-dev.xml" ||
		die "could not write bin/manifest-dev.xml"
	# monkey.jungle with the dev manifest; test builds add test/ (the helpers
	# and the (:test) suite).
	# Paths are relative to bin/.
	sources="../source"
	[[ $mode != test ]] || sources+=";../test"
	printf '%s\n' "# Generated by build.sh: monkey.jungle with the dev app id." \
		"project.manifest = manifest-dev.xml" "base.sourcePath = $sources" \
		"base.resourcePath = ../resources" >"$here/bin/dev.jungle"
	jungles="$here/bin/dev.jungle"
fi

args=(-y "$key" -l 3 -w -f "$jungles")
case $mode in
release)
	args+=(-d "$device")
	out="$here/bin/coachd.prg"
	;;
test)
	args+=(-d "$device" -t) # -t compiles the (:test) suite
	out="$here/bin/coachd-test.prg"
	;;
store)
	args+=(-e -r)
	out="$here/bin/coachd.iq"
	;;
store-beta)
	args+=(-e -r)
	out="$here/bin/coachd-beta.iq"
	;;
esac

echo "build.sh: $mode build${device:+ for $device} -> ${out#"$root"/}" >&2
"$sdk/bin/monkeyc" "${args[@]}" -o "$out"
