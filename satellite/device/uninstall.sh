# uninstall.sh - restore the stock Cast boot (run on the clock as root).
set -e
B=/sbin/busybox
rm -f /data/local/clockd/enabled
[ -f /system/bin/cast_installer.stock ] && $B mv /system/bin/cast_installer.stock /system/bin/cast_installer
[ -f /system/chrome/cast_cli.stock ] && $B mv /system/chrome/cast_cli.stock /system/chrome/cast_cli
sync
echo "stock boot restored (clockd files left in /data/local/clockd)"
