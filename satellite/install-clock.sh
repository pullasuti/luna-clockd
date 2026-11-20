#!/bin/sh
# install-clock.sh - install (or update) clockd on a rooted clock in one go, from
# the PC: build, package, push, install, optional Wi-Fi, start.
#
#	satellite/install-clock.sh [-s CLOCK] [--wifi SSID] [--hotspot-password] [--no-start] [--yes]
#	satellite/install-clock.sh [-s CLOCK] --latest | --release FILE | --build | --no-build [...]
#
# Without --latest, --release, --build or --no-build: a PC with the helpers built
# (satellite/build/bin, aec) builds from source; any other PC is offered the
# latest release instead (asks first; --yes takes it).
#
#	-s CLOCK      which clock, as adb names it: over Wi-Fi its address with
#	              the port (192.0.2.10:5555, after adb connect 192.0.2.10:5555),
#	              over USB the serial 'adb devices' shows. Needed only when
#	              adb sees more than one device (default: $ADB_SERIAL, or the
#	              only one)
#	--wifi SSID   also save this Wi-Fi network on the clock. The password is
#	              asked for (not echoed), or taken from $WIFI_PASSWORD; empty
#	              = an open network. Only the derived WPA key is sent to the
#	              clock, never the password itself.
#	--hotspot-password     give the clock's setup hotspot a password (WPA2)
#	              instead of leaving it open. Asked for (not echoed), or
#	              taken from $HOTSPOT_PASSWORD; 8 to 63 characters. Only the
#	              derived key is stored on the clock, so keep the password:
#	              the clock cannot show it, and without it the hotspot can't
#	              be joined (set a new one with this option over adb).
#	--no-hotspot-password  remove it: the setup hotspot is open again
#	--latest      download the latest release from the project's Releases page
#	              (package + SHA256SUMS, into build/downloads/) and install it as
#	              with --release. Needs curl or wget. $GITHUB_TOKEN, if set, is
#	              sent along (a private repository, or API rate limits)
#	--build       build clockd from source (Go 1.21+), without asking
#	--no-build    use the existing build/clockd-arm64
#	--no-start    don't restart the clock / clockd at the end
#	--yes         don't ask before installing
#	--release F   install a release package (clockd-<version>-arm64.tar.gz from the
#	              project's Releases page) instead of building: no Go needed, and
#	              it has every helper (setup hotspot firewall, ssh, WebRTC mic).
#	              This PC checks each file against the package's manifest (and its
#	              signature when Go is installed; else SHA256SUMS next to the
#	              file, if there). First install: staged with this checkout's
#	              install scripts. Update: the installed clockd (0.78.0+) checks
#	              and installs it, and rolls back if the new one doesn't start
#	--allow-downgrade  with --release: accept an older version
#	--tools       also install the optional tools (strace, tcpdump from
#	              tools/toolbox/build.sh) into /data/local/clockd/tools/bin;
#	              ssh logins have them first on PATH
#
# It only installs on a Lenovo Smart Clock Essential (model CD-4N341Y), shows
# which clock it found (MAC, name, installed version) and asks first.
#
# A first install restarts the clock (clockd starts at boot in place of Cast);
# an update just restarts clockd. Settings, keys and wake word models already
# on the clock are kept. Without a Wi-Fi network the clock opens its setup
# hotspot ("LenovoClock-XXXX") after boot.
set -e
S=$(cd "$(dirname "$0")" && pwd)
SERIAL=${ADB_SERIAL:-}
SSID= WIFI= BUILD=1 START=1 YES= RELEASE= DOWN= TOOLS= HOTPW= FRESH= LATEST= CHOSE=
# where releases come from (the public repository)
PROJECT=${CLOCKD_PROJECT:-https://github.com/pullasuti/luna-clockd}
while [ $# -gt 0 ]; do
	case "$1" in
	-s) SERIAL=$2; shift 2 ;;
	--wifi) SSID=$2; WIFI=1; shift 2 ;;
	--hotspot-password) HOTPW=set; shift ;;
	--no-hotspot-password) HOTPW=remove; shift ;;
	--no-build) BUILD=; CHOSE=1; shift ;;
	--build) BUILD=1; CHOSE=1; shift ;;
	--latest) LATEST=1; BUILD=; CHOSE=1; shift ;;
	--no-start) START=; shift ;;
	--yes) YES=1; shift ;;
	--release) RELEASE=$2; BUILD=; CHOSE=1; shift 2 ;;
	--allow-downgrade) DOWN=" -allow-downgrade"; shift ;;
	--tools) TOOLS=1; shift ;;
	-h|--help) sed -n "2,$(($(grep -n "^set -e" "$0" | cut -d: -f1) - 1))p" "$0"; exit 0 ;;
	*) echo "unknown option $1 (see --help)"; exit 2 ;;
	esac
done
# no choice made: offer the latest release unless this PC has the helpers to build with
if [ -z "$CHOSE" ] && { [ ! -f "$S/build/bin/iptables" ] || [ ! -f "$S/build/aec/apmd" ]; }; then
	echo "Recommended: the latest release from $PROJECT/releases"
	echo "(built and signed, with every helper). A build from source here would have no setup"
	echo "hotspot, ssh or WebRTC microphone: their helpers aren't built on this PC."
	if [ -n "$YES" ]; then
		ok=y
	else
		printf 'Download and install the latest release? [Y/n] (n: build from source anyway) '
		read -r ok || ok=
	fi
	case "$ok" in n|N|no) ;; *) LATEST=1 BUILD= ;; esac
fi
if [ -n "$LATEST" ]; then
	# the newest release's package and SHA256SUMS (GitHub API; JSON read without jq)
	get() { # url file [accept]
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} -H "Accept: ${3:-application/vnd.github+json}" -o "$2" "$1"
		elif command -v wget >/dev/null 2>&1; then
			wget -q ${GITHUB_TOKEN:+--header="Authorization: Bearer $GITHUB_TOKEN"} --header="Accept: ${3:-application/vnd.github+json}" -O "$2" "$1"
		else
			echo "Neither curl nor wget is installed; install one (e.g. sudo apt install curl), or download the package by hand and use --release."; exit 1
		fi
	}
	API=https://api.github.com/repos/${PROJECT#https://github.com/}
	mkdir -p "$S/build/downloads"
	J=$S/build/downloads/latest.json
	echo "Looking up the latest release ..."
	get "$API/releases/latest" "$J" ||
		{ echo "Could not reach $PROJECT/releases (offline, or the repository is private: set GITHUB_TOKEN). Nothing was installed."; exit 1; }
	# one "key": value per line
	tr ',{}[]' '\n\n\n\n\n' <"$J" | sed 's/^ *//; s/^\("[^"]*"\): */\1: /' >"$J.lines"
	TAG=$(sed -n 's/^"tag_name": *"\(.*\)"$/\1/p' "$J.lines" | head -n 1)
	V=${TAG#v}
	PKG=clockd-$V-arm64.tar.gz
	[ -n "$V" ] && grep -q "\"name\": *\"$PKG\"" "$J.lines" ||
		{ echo "The latest release ($TAG) has no $PKG. Nothing was installed."; exit 1; }
	D=$S/build/downloads/$V
	mkdir -p "$D"
	for n in "$PKG" SHA256SUMS; do
		[ -f "$D/$n" ] && [ "$n" = "$PKG" ] && continue
		# each asset's API url comes before its name; the API url works for private repositories too
		u=$(awk -v n="\"name\": \"$n\"" '/^"url": "https:\/\/api\.github\.com\/.*\/releases\/assets\//{u=$0} $0 == n {print u; exit}' "$J.lines" |
			sed 's/^"url": *"\(.*\)"$/\1/')
		[ -n "$u" ] || { echo "The latest release ($TAG) has no $n. Nothing was installed."; exit 1; }
		echo "Downloading $n ..."
		get "$u" "$D/$n.part" application/octet-stream && mv -f "$D/$n.part" "$D/$n" ||
			{ rm -f "$D/$n.part"; echo "Downloading $n failed. Nothing was installed; try again."; exit 1; }
	done
	RELEASE=$D/$PKG
fi
if [ -n "$RELEASE" ]; then
	[ -f "$RELEASE" ] || { echo "no such package: $RELEASE"; exit 2; }
	RELEASE=$(cd "$(dirname "$RELEASE")" && pwd)/${RELEASE##*/} # the checks below run elsewhere
fi
if [ -n "$TOOLS" ] && ! ls "$S"/build/tools/bin/* >/dev/null 2>&1; then
	echo "no optional tools in $S/build/tools/bin (build them with tools/toolbox/build.sh)"; exit 2
fi
# the PC tools, before anything touches the clock
if ! command -v adb >/dev/null 2>&1; then
	cat <<-EOF
	adb (Android platform tools) is not installed; it talks to the clock. Install it with:
	  Debian, Ubuntu:  sudo apt install adb
	  Fedora:          sudo dnf install android-tools
	  Arch:            sudo pacman -S android-tools
	  macOS:           brew install android-platform-tools
	  or Google's download: https://developer.android.com/tools/releases/platform-tools
	Then run this again. Nothing was installed.
	EOF
	exit 1
fi
if [ -n "$BUILD" ]; then
	GOV=$(go env GOVERSION 2>/dev/null || true) # go1.24.4
	GOMIN=$(echo "$GOV" | sed -n 's/^go1\.\([0-9]*\).*/\1/p')
	if [ -z "$GOMIN" ] || [ "$GOMIN" -lt 21 ]; then
		[ -n "$GOV" ] && echo "Go is too old here ($GOV); clockd needs Go 1.21 or newer to build."
		[ -z "$GOV" ] && echo "Go is not installed; it builds clockd on this PC."
		cat <<-EOF
		Install it with:
		  Debian, Ubuntu:  sudo apt install golang-go
		  Fedora:          sudo dnf install golang
		  Arch:            sudo pacman -S go
		  macOS:           brew install go
		  or the official download: https://go.dev/dl/
		Any Go from 1.21 on works: the first build fetches the exact version this project uses
		($(sed -n 's/^go //p' "$S/go.mod")) by itself. Then run this again. Nothing was installed.
		EOF
		exit 1
	fi
fi
adb() { command adb ${SERIAL:+-s "$SERIAL"} "$@" </dev/null; } # adb shell would eat the answers on stdin

# a release package: unpack and check it here first (also before an update)
PKGDIR= PKGVER=
if [ -n "$RELEASE" ]; then
	sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
	PKGDIR=$(mktemp -d "${TMPDIR:-/tmp}/clockd-pkg.XXXXXX")
	trap 'rm -rf "$PKGDIR"' EXIT
	tar -xzf "$RELEASE" -C "$PKGDIR" 2>/dev/null && [ -f "$PKGDIR/manifest.json" ] ||
		{ echo "$RELEASE is not a clockd release package (no manifest). Nothing was installed."; exit 1; }
	M=$PKGDIR/manifest.json
	PKGVER=$(sed -n 's/^  "version": "\(.*\)",$/\1/p' "$M")
	grep -q '"product": "clockd"' "$M" && grep -q '"model": "CD-4N341Y"' "$M" && [ -n "$PKGVER" ] ||
		{ echo "$RELEASE is not a clockd package for this clock. Nothing was installed."; exit 1; }
	# every file in the manifest, with its SHA-256; nothing else gets installed
	awk '/^      "path": /{gsub(/^ *"path": "|",$/, ""); p=$0} /^      "sha256": /{gsub(/^ *"sha256": "|",$/, ""); print $0, p}' "$M" >"$PKGDIR/sums"
	[ -s "$PKGDIR/sums" ] || { echo "$RELEASE: empty manifest. Nothing was installed."; exit 1; }
	while read -r want f; do
		case "$f" in /*|*..*) echo "$RELEASE: bad path $f. Nothing was installed."; exit 1 ;; esac
		[ -f "$PKGDIR/files/$f" ] && [ "$(sha "$PKGDIR/files/$f")" = "$want" ] ||
			{ echo "$RELEASE is damaged ($f does not match its manifest). Download it again. Nothing was installed."; exit 1; }
	done <"$PKGDIR/sums"
	if command -v go >/dev/null 2>&1; then
		(cd "$S" && GOTOOLCHAIN=auto go run ./tools/release verify "$RELEASE" >/dev/null 2>&1) ||
			{ echo "$RELEASE: the signature does not check out. Nothing was installed."; exit 1; }
		PKGCHECK="signature and files checked"
	elif [ -f "$(dirname "$RELEASE")/SHA256SUMS" ]; then
		(cd "$(dirname "$RELEASE")" && grep " ${RELEASE##*/}\$" SHA256SUMS >"$PKGDIR/s1" && [ -s "$PKGDIR/s1" ] &&
			[ "$(sha "${RELEASE##*/}")" = "$(cut -d' ' -f1 "$PKGDIR/s1")" ]) ||
			{ echo "$RELEASE does not match the SHA256SUMS next to it. Download both again. Nothing was installed."; exit 1; }
		PKGCHECK="files checked; matches SHA256SUMS (no Go here for the signature)"
	else
		PKGCHECK="files checked (no Go here for the signature, no SHA256SUMS next to it)"
	fi
fi
step() { printf '\n== %s\n' "$*"; }

step "Looking for the clock"
# the -s hint, with what adb sees (when several devices or the wrong one answered)
which_clock() {
	echo "Choose the clock with -s: over Wi-Fi its address and port (-s <clock address>:5555),"
	echo "over USB its serial. adb sees these:"
	command adb devices -l </dev/null | sed -n '2,$p' | sed '/^$/d; s/^/  /'
	echo "The clock's address is on its setup page once it has joined Wi-Fi, in its web UI's"
	echo "address bar, or in your router's device list."
}
adb get-state >/dev/null 2>&1 || {
	if [ -z "$SERIAL" ] && [ "$(command adb devices </dev/null | awk 'NR > 1 && $2 == "device"' | wc -l)" -gt 1 ]; then
		echo "adb sees more than one device. Nothing was installed."
		which_clock
		exit 1
	fi
	echo "No clock found over adb${SERIAL:+ (looked for $SERIAL)}."
	echo "Connect the clock with a USB cable, or over Wi-Fi with: adb connect <clock address>:5555"
	echo "Then check that 'adb devices' lists it, and run this script again${SERIAL:+ (-s takes the name it shows there)}."
	exit 1
}
case "$(adb shell id 2>/dev/null)" in
*uid=0*) ;;
*) echo "adb works, but not as root on this clock. Unlock and root the clock first, then run this again."; exit 1 ;;
esac
prop() { adb shell getprop "$1" 2>/dev/null | tr -d '\r'; }
MODEL=$(prop ro.product.model)
has() { [ "$(adb shell "/sbin/busybox test -e $1 && echo yes" 2>/dev/null | tr -d '\r')" = yes ]; } # this adb drops exit codes
if [ "$MODEL" != "Lenovo CD-4N341Y" ] || ! has /system/bin/cast_installer || ! has /sys/class/leds/night_light; then
	echo "This device is not a Lenovo Smart Clock Essential (it reports: ${MODEL:-?}, $(prop ro.product.manufacturer) $(prop ro.product.device))."
	echo "Nothing was installed."
	which_clock
	exit 1
fi
MAC=$(adb shell cat /sys/class/net/wlan0/address 2>/dev/null | tr -d '\r\n ')
NAME=$(adb shell cat /data/local/clockd/state.json 2>/dev/null | tr -d '\r' | sed -n 's/.*"device_name": *"\([^"]*\)".*/\1/p')
VER=$(adb shell "grep 'clockd running' /tmp/clockd.log 2>/dev/null" | tr -d '\r' | tail -n 1 | sed -n 's/.*version=\([^ ]*\).*/\1/p') # the clock's tail ignores -1
IP=$(adb shell "ifconfig wlan0 2>/dev/null" | tr -d '\r' | sed -n 's/.*inet addr:\([0-9.]*\).*/\1/p' | head -n 1)
HOTNAME=LenovoClock-$(printf '%s' "$MAC" | tr -d ':' | tail -c 4 | tr a-f A-F) # clockd names.go hotspotName
echo "Found a Lenovo Smart Clock Essential:"
echo "  Model:     $MODEL, firmware $(prop ro.build.display.id)"
echo "  adb:       ${SERIAL:-$(command adb devices | awk 'NR == 2 {print $1}')}"
echo "  Wi-Fi MAC: $MAC"
echo "  Address:   ${IP:-not on Wi-Fi}"
if has /data/local/clockd/clockd; then
	echo "  clockd:    installed (${NAME:-lenovo-clock}, version ${VER:-? - not running})"
	echo
	echo "This will UPDATE clockd. Settings, keys and wake words stay; only clockd restarts."
else
	[ -z "$RELEASE" ] || FRESH=1
	echo "  clockd:    not installed"
	echo
	echo "This is a FIRST INSTALL: clockd replaces Cast, and the clock restarts at the end."
fi
if [ -n "$RELEASE" ]; then
	echo "Package: $RELEASE (clockd $PKGVER; $PKGCHECK)"
	REPOVER=$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$S/cmd/clockd/main.go")
	if [ -n "$FRESH" ] && [ "$REPOVER" != "$PKGVER" ]; then
		echo "Note: the install scripts come from this checkout ($REPOVER), the package is $PKGVER."
		echo "      For an exact match, use the source code of the same release."
	fi
fi
if [ -z "$YES" ]; then
	printf 'Install on this clock? Type y and press Enter: '
	read -r ok || ok=
	case "$ok" in y|Y|yes) ;; *) echo "Stopped. Nothing was changed on the clock."; exit 1 ;; esac
fi

HOTPASS=
if [ "$HOTPW" = set ]; then
	if [ -n "${HOTSPOT_PASSWORD+x}" ]; then
		HOTPASS=$HOTSPOT_PASSWORD
	else
		printf 'Password for the setup hotspot (hidden while you type): '
		stty -echo 2>/dev/null || true
		read -r HOTPASS || true
		stty echo 2>/dev/null || true
		echo
		printf 'Once more: '
		stty -echo 2>/dev/null || true
		read -r HOTPASS2 || true
		stty echo 2>/dev/null || true
		echo
		[ "$HOTPASS" = "$HOTPASS2" ] || { echo "The two don't match. Nothing was changed; run this again."; exit 2; }
	fi
	if [ ${#HOTPASS} -lt 8 ] || [ ${#HOTPASS} -gt 63 ]; then
		echo "A Wi-Fi (WPA) password is 8 to 63 characters. Nothing was changed; run this again."; exit 2
	fi
fi
PASS=
if [ -n "$WIFI" ]; then
	[ -n "$SSID" ] || { echo "--wifi needs a network name"; exit 2; }
	if [ -n "${WIFI_PASSWORD+x}" ]; then
		PASS=$WIFI_PASSWORD
	else
		printf 'Password for Wi-Fi "%s" (hidden while you type; just Enter for an open network): ' "$SSID"
		stty -echo 2>/dev/null || true
		read -r PASS || true
		stty echo 2>/dev/null || true
		echo
	fi
	if [ -n "$PASS" ] && { [ ${#PASS} -lt 8 ] || [ ${#PASS} -gt 63 ]; }; then
		echo "A Wi-Fi (WPA) password is 8 to 63 characters. Nothing was changed; run this again."; exit 2
	fi
fi

if [ -n "$BUILD" ]; then
	step "Building clockd"
	# GOTOOLCHAIN=auto: an older Go (distro packages often set "local") fetches the one go.mod names
	echo "(the first build downloads Go $(sed -n 's/^go //p' "$S/go.mod") if needed, and the libraries: a minute or two)"
	(cd "$S" && GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o build/clockd-arm64 ./cmd/clockd)
fi

if [ -n "$RELEASE" ] && [ -z "$FRESH" ]; then
	step "Installing the release package"
	P=/data/local/tmp/clockd-release.tar.gz
	adb push "$RELEASE" $P >/dev/null 2>&1 || { echo "Copying the package to the clock failed. Nothing was installed; check the connection and try again."; exit 1; }
	[ "$(adb shell "wc -c < $P" | tr -d '\r ')" = "$(wc -c < "$RELEASE" | tr -d ' ')" ] || { echo "The package arrived incomplete. Nothing was installed; try again (USB is more reliable than Wi-Fi)."; adb shell rm -f $P; exit 1; }
	OUT=$(adb shell "/data/local/clockd/clockd$DOWN -install $P 2>&1" | tr -d '\r')
	adb shell rm -f $P
	echo "$OUT" | sed 's/^/  /'
	case "$OUT" in # errors first: their text can contain the success words
	*"not defined: -install"*) echo "Not installed: the clockd on the clock is older than 0.78.0. Update it once without --release."; exit 1 ;;
	*"older than the installed"*) echo "Not installed: this package is older than the clockd on the clock. Add --allow-downgrade to go back."; exit 1 ;;
	*"clockd: "*) echo "Not installed (reason above). The clock keeps its current clockd."; exit 1 ;;
	*"restart clockd to switch"*) OUT="$OUT restart.sh" ;;
	*"nothing changed"*) START= ;;
	*) echo "Not installed (reason above). The clock keeps its current clockd."; exit 1 ;;
	esac
else
	if [ -n "$FRESH" ]; then
		# the install scripts of this checkout plus the package's files (what stage.sh collects)
		step "Packaging (from the release package)"
		O=$S/build/clockd-device
		rm -rf "$O" && mkdir -p "$O"
		cp -r "$S/device/." "$O/"
		rm -f "$O/stage.sh"
		while read -r want f; do
			mkdir -p "$O/$(dirname "$f")"
			cp "$PKGDIR/files/$f" "$O/$f"
		done <"$PKGDIR/sums"
		echo "  ready: clockd $PKGVER, $(wc -l <"$PKGDIR/sums" | tr -d ' ') files"
	else
		step "Packaging"
		sh "$S/device/stage.sh" | grep -v -e '^  ' -e '^staged in' | sed 's/^/  /'
		echo "  ready"
	fi

	step "Copying to the clock and installing"
	adb shell rm -rf /data/local/tmp/clockd-device
	adb push "$S/build/clockd-device" /data/local/tmp/clockd-device >/dev/null 2>&1
	OUT=$(adb shell /sbin/busybox sh /data/local/tmp/clockd-device/install.sh | tr -d '\r')
	echo "$OUT" | grep -v '^installed;' | sed 's/^/  /' # the last line names on-clock commands this script runs itself
	adb shell rm -rf /data/local/tmp/clockd-device
	case "$OUT" in *"installed;"*) ;; *) echo "The install on the clock failed (output above). The clock keeps running what it had."; exit 1 ;; esac
fi

if [ -n "$WIFI" ]; then
	step "Saving Wi-Fi network \"$SSID\" on the clock"
	# hex SSID (any characters), WPA key derived here: the password stays on the PC
	HEX=$(printf '%s' "$SSID" | od -An -tx1 | tr -d ' \n')
	PSK=
	if [ -n "$PASS" ]; then
		PSK=$(SSID="$SSID" PASS="$PASS" python3 -c 'import hashlib,os; print(hashlib.pbkdf2_hmac("sha1", os.environ["PASS"].encode(), os.environ["SSID"].encode(), 4096, 32).hex())')
	fi
	W="/system/bin/wpa_cli -p /data/wifi -i wlan0"
	# drop older copies of this network, then add it
	for id in $(adb shell "$W list_networks" | tr -d '\r' | awk -F'\t' -v s="$SSID" 'NR > 1 && $2 == s {print $1}'); do
		adb shell "$W remove_network $id" >/dev/null
	done
	ID=$(adb shell "$W add_network" | tr -d '\r' | tail -1)
	case "$ID" in ''|*[!0-9]*) echo "add_network failed: $ID"; exit 1 ;; esac
	set_net() { r=$(adb shell "$W set_network $ID $1 $2" | tr -d '\r'); [ "$r" = OK ] || { echo "set_network $1: $r"; exit 1; }; }
	set_net ssid "$HEX"
	if [ -n "$PSK" ]; then
		set_net psk "$PSK"
		set_net key_mgmt WPA-PSK
	else
		set_net key_mgmt NONE
	fi
	set_net scan_ssid 1
	adb shell "$W enable_network $ID" >/dev/null
	[ "$(adb shell "$W save_config" | tr -d '\r')" = OK ] || { echo "save_config failed"; exit 1; }
	echo "  Saved. The clock joins it after the restart (unless it is already on another saved network)."
fi

if [ "$HOTPW" = set ]; then
	step "Setting the setup hotspot's password"
	# the key depends on the hotspot's name; derived here, the password stays on the PC
	HOTKEY=$(SSID="$HOTNAME" PASS="$HOTPASS" python3 -c 'import hashlib,os; print(hashlib.pbkdf2_hmac("sha1", os.environ["PASS"].encode(), os.environ["SSID"].encode(), 4096, 32).hex())')
	[ ${#HOTKEY} -eq 64 ] || { echo "Deriving the key failed (python3 is needed). The hotspot is unchanged."; exit 1; }
	K=/data/local/clockd/hotspot.key
	adb shell "/sbin/busybox sh -c 'umask 077; echo $HOTKEY > $K.tmp && /sbin/busybox mv -f $K.tmp $K'"
	[ "$(adb shell "wc -c < $K" | tr -d '\r ')" = 65 ] || { echo "Writing the key to the clock failed."; exit 1; }
	echo "  Done. The setup hotspot \"$HOTNAME\" now asks for that password (from its next start)."
	echo "  Keep the password: the clock can't show it."
elif [ "$HOTPW" = remove ]; then
	step "Removing the setup hotspot's password"
	adb shell "rm -f /data/local/clockd/hotspot.key"
	echo "  Done. The setup hotspot is open (from its next start)."
fi

if [ -n "$TOOLS" ]; then
	step "Installing the optional tools"
	T=/data/local/clockd/tools/bin
	adb shell "mkdir -p $T"
	for f in "$S"/build/tools/bin/*; do
		n=${f##*/}
		adb push "$f" "$T/$n.tmp" >/dev/null 2>&1 || { echo "Copying $n failed."; exit 1; }
		[ "$(adb shell "wc -c < $T/$n.tmp" | tr -d '\r ')" = "$(wc -c < "$f" | tr -d ' ')" ] || { echo "$n arrived incomplete; try again."; adb shell rm -f "$T/$n.tmp"; exit 1; }
		adb shell "chmod 755 $T/$n.tmp && /sbin/busybox mv -f $T/$n.tmp $T/$n"
		echo "  $n"
	done
fi

KEY=$(adb shell cat /data/local/clockd/api.key 2>/dev/null | tr -d '\r\n')
FIRST=
if [ -z "$START" ]; then
	step "Installed, not started (--no-start)"
elif echo "$OUT" | grep -q "restart.sh"; then
	step "Restarting clockd"
	adb shell /sbin/busybox sh /data/local/clockd/restart.sh | tail -1 | cut -c1-100 | sed 's/^/  /'
else
	FIRST=1
	step "Restarting the clock"
	adb shell /system/bin/reboot >/dev/null 2>&1 || adb reboot
	echo "  The clock is restarting. clockd starts about 15 s after it has booted."
fi
SFX=$(printf '%s' "$MAC" | tr -d ':' | tail -c 4)
HOT=LenovoClock-$(printf '%s' "$SFX" | tr a-f A-F)
step "Done. What to do next"
n=1
WEB=http://${NAME:-lenovo-clock-$SFX}.local/
if [ -n "$FIRST" ] && [ -z "$WIFI" ] && [ -z "$IP" ]; then
	echo "$n. Join the clock's setup Wi-Fi from a phone or computer:"
	if [ "$HOTPW" = set ] || [ -n "$(adb shell "ls /data/local/clockd/hotspot.key 2>/dev/null")" ]; then
		echo "     Wi-Fi name:  $HOT  (the hotspot password you set)"
	else
		echo "     Wi-Fi name:  $HOT  (no password)"
	fi
	echo "   Join once the clock's face scrolls \"setup\" (shortly after the restart): your"
	echo "   computer may still list the name from before the restart, and joining before the"
	echo "   clock is up fails. On a phone the setup page should open by itself; if not, open"
	echo "   http://192.168.255.249/"
	n=$((n + 1))
	echo "$n. The setup page asks you to hold the action button on top of the clock for a second."
	n=$((n + 1))
	echo "$n. Then choose your own Wi-Fi network on the page. When the clock has joined it, the"
	echo "   page shows a link to the clock's web page. The login page there shows which button"
	echo "   on the clock to press."
else
	[ -z "$WIFI" ] || IP= # it may move to the new network
	echo "$n. Open the clock's web page: ${IP:+http://$IP/ or }$WEB"
	[ -n "$IP" ] || echo "   (about a minute after the restart; if the name doesn't open, find the clock in your"
	[ -n "$IP" ] || echo "   router's list of devices, where it shows up as \"lenovo\")"
	echo "   The login page shows which button on the clock to press."
fi
n=$((n + 1))
if [ -n "$FIRST" ] || [ -z "$KEY" ]; then
	echo "$n. Add it to Home Assistant: Settings > Devices & services. The clock shows up as a"
	echo "   discovered ESPHome device; HA then asks for the ESPHome encryption key. Find the key"
	echo "   on the clock's web page (Home Assistant tab), or print it with:"
	echo "     adb${SERIAL:+ -s $SERIAL} shell cat /data/local/clockd/api.key"
	[ -n "$KEY" ] || echo "   (on a first install the key is created when clockd first starts)"
else
	echo "$n. Home Assistant reconnects to the clock by itself; nothing to do there."
fi
