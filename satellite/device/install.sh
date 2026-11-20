# install.sh - install clockd and make it start at boot instead of Cast (run on
# the clock as root). From the PC:
#   satellite/device/stage.sh                 (collects clockd + helpers)
#   adb push satellite/build/clockd-device /data/local/tmp/clockd-device
#   adb shell /sbin/busybox sh /data/local/tmp/clockd-device/install.sh
# Idempotent; also the way to update the helpers. Stock binaries are kept as
# *.stock; uninstall.sh restores them. Settings, keys and models already in
# /data/local/clockd are never touched: missing wake word models are added,
# existing ones (and ones downloaded in the web UI) are left as they are.
# F=<dir> installs somewhere else (testing the fresh-install path).
set -e
B=/sbin/busybox
# only on a Lenovo Smart Clock Essential (this replaces its Cast launcher)
if [ "$(getprop ro.product.model)" != "Lenovo CD-4N341Y" ] || [ ! -f /system/bin/cast_installer ]; then
	echo "not a Lenovo Smart Clock Essential (model: $(getprop ro.product.model)): not installing"
	exit 1
fi
D=$(cd "$(dirname "$0")" && pwd)
F=${F:-/data/local/clockd}
mkdir -p $F
# a running clockd is replaced by restart.sh; a first install just puts it in place
running=
if [ -f $F/clockd ] && [ -f /tmp/clockd.launcher ] && kill -0 "$(cat /tmp/clockd.launcher)" 2>/dev/null; then
	running=1
fi
if [ -f $D/clockd ]; then
	if [ -n "$running" ]; then
		cp $D/clockd $F/clockd.new && chmod 755 $F/clockd.new
	else
		cp $D/clockd $F/clockd && chmod 755 $F/clockd
	fi
fi
[ -x $F/clockd ] || { echo "missing clockd (run stage.sh, or push the arm64 build to $F/clockd)"; exit 1; }
for f in start.sh restart.sh setup-ap.sh; do
	[ -f $D/$f ] && cp $D/$f $F/$f
done
# wake word models: add the missing ones
if [ -d $D/models ]; then
	mkdir -p $F/models
	n=0
	for m in $D/models/*; do
		[ -f "$F/models/${m##*/}" ] && continue
		$B cp "$m" $F/models/ && chmod 644 "$F/models/${m##*/}" && n=$((n + 1))
	done
	[ $n -gt 0 ] && echo "installed $n wake word model files"
fi
ls $F/models/*.json >/dev/null 2>&1 || { echo "no wake word models in $F/models (run stage.sh)"; exit 1; }
# helpers: setup hotspot firewall, busybox, ssh, microphone processing (optional)
for f in bin/iptables bin/busybox bin/dropbearmulti bin/dhcp.sh aec/apmd; do
	if [ -f $D/$f ]; then
		mkdir -p $F/$(dirname $f)
		# copy, then rename: a running helper can't be overwritten ("text file busy")
		$B cp $D/$f $F/$f.tmp
		case $f in *.gguf) chmod 644 $F/$f.tmp ;; *) chmod 755 $F/$f.tmp ;; esac
		$B mv -f $F/$f.tmp $F/$f
		echo "installed $f"
	fi
done
# ssh (dropbear): host keys and authorized_keys live here; root only
mkdir -p $F/ssh && chmod 700 $F/ssh
cd /system/bin
[ -f cast_installer.stock ] || $B mv cast_installer cast_installer.stock
cp $D/system/cast_installer cast_installer && chmod 755 cast_installer
cd /system/chrome
[ -f cast_cli.stock ] || $B mv cast_cli cast_cli.stock
cp $D/system/cast_cli cast_cli && chmod 755 cast_cli
touch $F/enabled
sync
if [ -f $F/clockd.new ]; then
	echo "installed; switch to the new clockd with: $B sh $F/restart.sh"
else
	echo "installed; clockd starts at next boot (rm $F/enabled to boot stock Cast)"
fi
