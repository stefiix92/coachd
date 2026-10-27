---
title: Development
weight: 4
---

Building the watch app yourself is for development and for the Store uploads; runners install it from the Store. The source is in [`watch/`](https://github.com/stefiix92/coachd/tree/main/watch).

## Prerequisites

- **Connect IQ SDK 9.3.0** unpacked at `~/.cache/connectiq/sdk-9.3.0`, or point `CIQ_SDK` at another SDK directory, such as one the SDK Manager installed under `~/Library/Application Support/Garmin/ConnectIQ/Sdks/`. The builds use `$CIQ_SDK/bin/monkeyc`, which runs from the Linux or the macOS SDK alike. The simulator (needed only for the tests) runs from the macOS SDK, or from the Linux SDK on Ubuntu 22.04, the last release with the `webkit2gtk-4.0` it links.
- **Java 17** on `PATH` (monkeyc is a Java program), for example `brew install openjdk@17` and `export PATH="/opt/homebrew/opt/openjdk@17/bin:$PATH"`.
- **Device files.** They come from the Connect IQ SDK Manager and need a Garmin developer login: sign in and download the devices; they land in `~/Library/Application Support/Garmin/ConnectIQ/Devices/` (`~/.Garmin/ConnectIQ/Devices/` on Linux). A sideload or test build needs its one device (default `fenix7spro`; a test build falls back to `fenix6pro`). A Store build needs every device in `manifest.xml` and lists the missing ones.

## Build

```bash
cd watch
COACHD_PUBLIC_URL=https://coachd.example.com ./build.sh        # bin/coachd.prg, sideload
COACHD_PUBLIC_URL=https://coachd.example.com ./build.sh test   # bin/coachd-test.prg
./build.sh store                                               # bin/coachd.iq, the public Store app
./build.sh store-beta                                          # bin/coachd-beta.iq, the Store beta
```

- **Two app ids.** The public id in `manifest.xml` ("coachd") is used only by `store`. Sideload, test and `store-beta` builds use a dev id ("coachd dev"), so they sit next to the Store install instead of replacing it; a Store beta cannot use the public app's id. `build.sh` writes the dev manifest and `bin/dev.jungle` on every build and never edits tracked files.
- **The URL.** Sideloaded apps get no phone settings, so sideload and test builds compile `COACHD_PUBLIC_URL` into `source/Config.mc` (generated from `source/Config.mc.example`, git-ignored) as the setting's default; it must be `https://`. Store builds take no URL and embed none (`build.sh` refuses a set `COACHD_PUBLIC_URL`).
- `CIQ_DEVICE` picks the device of a sideload or test build: any product in `manifest.xml`.
- Builds use strict type checking (`-l 3`). Store builds warn that `:glance` is ignored on watches without glances; that is expected.

{{< callout type="warning" >}}
The first build creates the developer key `.secrets/ciq-developer.der` at the repo root (RSA 4096, PKCS#8 DER, mode 0600, git-ignored). Every build is signed with it, Store uploads included. Keep it and back it up: an update signed with another key is another app.
{{< /callout >}}

## Supported devices

`./devices.sh` lists the installed fēnix and Forerunner device files that run watch apps at API level 3.0 or later and have app workouts and FIT downloads, which the sync needs. Paste its output into `<iq:products>` in `manifest.xml` after downloading new devices.

## Sideload

The watch is an MTP device, so the Mac needs [OpenMTP](https://openmtp.ganeshrvel.com/) (`brew install --cask openmtp`) or Android File Transfer.

{{% steps %}}

### Free the USB connection

Quit Garmin Express, and any app that grabs USB devices: Chrome with a WebUSB tab or the ChatGPT desktop app take the watch for themselves, and OpenMTP then sees nothing.

### Connect the watch

Connect it with its USB cable, directly to the Mac: a dock or hub may pass only power. Check that the Mac sees it (Garmin's USB vendor id is 0x091e): `ioreg -p IOUSB -l -w0 | grep -c '"idVendor" = 2334'` prints 1. If it does but OpenMTP still shows nothing, `killall ptpcamerad` and press Refresh.

### Copy the app

Open the watch in OpenMTP and copy `watch/bin/coachd.prg` into `GARMIN/APPS/`.

### Disconnect

The app appears in the apps list as "coachd dev".

{{% /steps %}}

To update, copy the new `coachd.prg` over the old one. A `coachd.prg` from a build before the Store release used the public id: it is a separate app, so delete it from `GARMIN/APPS/`. If the app does not start, look in `GARMIN/APPS/LOGS/CIQ_LOG.YML` on the watch.

## Store upload

The listing text and the upload checklist (beta first, the public app only after the owner approves the beta) are in [`watch/STORE.md`](https://github.com/stefiix92/coachd/blob/main/watch/STORE.md).

## Tests

The `(:test)` suite covers the sync, pairing and URL logic and the formatting, and smoke-tests every screen, the glance and the background notification. It runs only in the Connect IQ simulator:

```bash
export CIQ_SDK="$HOME/Library/Application Support/Garmin/ConnectIQ/Sdks/<macOS SDK 9.3.0 directory>"
"$CIQ_SDK/bin/connectiq" &             # starts the simulator
./simtest.sh fenix7spro fenix6pro      # test build and run per device
```

`simtest.sh` builds with `build.sh test` for each device, runs the 40 tests with `monkeydo` and fails unless each run ends with `PASSED` (`monkeydo` itself exits 1 even then). `build.sh test` adds `test/` to the source path; the app build leaves it out.

## CI

The `watch` workflow runs on changes under `watch/`: it builds the Store package for every product in `manifest.xml` and runs `simtest.sh` on a fēnix 7S Pro and a Forerunner 55 in the Linux simulator under Xvfb. `ci-setup.sh` downloads the Linux SDK from Garmin on every run; nothing of Garmin's is kept in the repository. The device files need a Garmin account, so set these repository secrets:

- `GARMIN_USERNAME` and `GARMIN_PASSWORD`: a Garmin account without two-step verification, ideally one used only for this. The workflow caches the device files, so it signs in only when the cache is empty; run it once on `main` (Actions, watch, Run workflow) to fill the cache. Without the secrets or a cache, as in forks, it skips the build with a notice.
- `CIQ_DEVELOPER_KEY` (optional), the developer key: `base64 < .secrets/ciq-developer.der | gh secret set CIQ_DEVELOPER_KEY`. With it, the release workflow attaches the public Store package `coachd.iq` to each GitHub release. Anyone with write access to the repository can read a secret through a workflow, and this key signs every update of the Store app.

`ci-setup.sh` accepts the Connect IQ SDK License Agreement for that account, but only the version whose hash it pins; when Garmin changes the agreement, CI fails until you read the new one and update the hash.
