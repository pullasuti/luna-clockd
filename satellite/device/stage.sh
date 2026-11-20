#!/bin/sh
# stage.sh - collect everything install.sh needs into one folder (on the PC):
#
#	satellite/device/stage.sh            -> satellite/build/clockd-device/
#	adb push satellite/build/clockd-device /data/local/tmp/clockd-device
#	adb shell /sbin/busybox sh /data/local/tmp/clockd-device/install.sh
#
# Needs (in satellite/build/, git-ignored):
#	clockd-arm64          go build (see README)
#	bin/iptables          tools/firewall/build-iptables.sh  (setup hotspot firewall)
#	bin/busybox, bin/dropbearmulti  tools/toolbox/build.sh  (current busybox; key-only ssh)
#	aec/apmd              tools/aec/build-apmd.sh           (WebRTC microphone source)
# and the wake word models from satellite/models/ (required: clockd won't start
# without them).
# Missing optional pieces are skipped with a note: the clock then falls back
# (no setup hotspot / the Original microphone source).
set -e
S=$(cd "$(dirname "$0")/.." && pwd)
B=$S/build
O=$B/clockd-device
[ -f "$B/clockd-arm64" ] || { echo "missing $B/clockd-arm64 (build clockd first)"; exit 1; }
rm -rf "$O"
mkdir -p "$O"
cp -r "$S/device/." "$O/"
rm -f "$O/stage.sh"
cp "$B/clockd-arm64" "$O/clockd"
ls "$S"/models/*.json >/dev/null 2>&1 || { echo "missing $S/models/*.json (wake word models)"; exit 1; }
mkdir -p "$O/models"
cp "$S"/models/*.json "$S"/models/*.tflite "$O/models/"
for f in bin/iptables bin/busybox bin/dropbearmulti aec/apmd; do
	if [ -f "$B/$f" ]; then
		mkdir -p "$O/$(dirname $f)"
		cp "$B/$f" "$O/$f"
	else
		echo "note: no $B/$f (skipped)"
	fi
done
echo "staged in $O:"
(cd "$O" && find . -type f | sort | sed 's/^\.\//  /')
