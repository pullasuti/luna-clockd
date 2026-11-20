# /data/local/clockd/start.sh - boot launcher for clockd (runs as root, from
# the cast_installer wrapper). Updatable without touching /system.
#
# Disable clockd at next boot:  rm /data/local/clockd/enabled   (stock Cast returns)
F=/data/local/clockd
LOG=/tmp/clockd.log

# only one launcher (boot_complete.sh may call cast_installer again)
if [ -f /tmp/clockd.launcher ] && kill -0 "$(cat /tmp/clockd.launcher)" 2>/dev/null; then
    exit 0
fi
echo $$ > /tmp/clockd.launcher

# boot_complete.sh restarts Cast unless this socket path exists
[ -e /tmp/cast-control ] || touch /tmp/cast-control

# Vendor daemons we replace or don't want (network, OTA, display, buttons, LEDs).
# Kept: adbd, logd, ueventd, servicemanager, watchdog, wpa_supplicant,
#       dhcpcd, console; iot_audio (vendor AEC front end) is handled below.
UNWANTED="process_monitor cast_receiver sntpd ota_service lenovo_status lenovo_alarm
 local_alarm external_control fdr_notify audio_source burning collectd panel_service
 input_service mute_service sshd bluetoothtbd" # BT serves only the Cast stack; frees ~4 MB and /dev/ttyS1
# net_mgr (vendor Wi-Fi command server: Google connectivity checks, OTA URLs) is only
# stopped, not removed: setup-ap.sh runs the hotspot itself. To go back to the old
# route (setup-ap.sh through net_mgr's socket):  touch /data/local/clockd/use-net_mgr
[ -e $F/use-net_mgr ] || UNWANTED="$UNWANTED net_mgr"
# mountd (/sbin/mount_usb_drive.sh) polls for a USB stick every 5 s to mount it at
# /data/usb, but the kernel runs the USB-A port as a USB device (adb, burn mode)
# unless it is switched to host by hand (Engineering Notes, 2026-11-10 USB host
# mode), so by default no stick appears.
# To keep it running:  touch /data/local/clockd/use-mountd
[ -e $F/use-mountd ] || UNWANTED="$UNWANTED mountd"
# DHCP: our busybox udhcpc (bin/dhcp.sh) instead of the vendor dhcpcd 6.8.2, which
# sends the fixed name "lenovo"/"audiocast" and puts Google's DNS first. dhcpcd is only
# stopped (init kills it, so the address stays while udhcpc asks for it again).
# Back to dhcpcd:  touch /data/local/clockd/use-dhcpcd
OWN_DHCP=
if [ ! -e $F/use-dhcpcd ] && [ -x $F/bin/busybox ] && [ -f $F/bin/dhcp.sh ]; then
    OWN_DHCP=1
    UNWANTED="$UNWANTED dhcpcd"
fi

# CPU: the stock "performance" governor pins all cores at 1.2 GHz; clockd's
# load (~13 % of the 4 cores) runs fine at 667 MHz-1 GHz, so use "interactive"
# (the only other one this kernel has): cooler, same response.
cpu_governor() {
    for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
        grep -q interactive ${g%/*}/scaling_available_governors 2>/dev/null && echo interactive > $g
    done
}

# Second shell next to adb: our dropbear (key-only, root, port 22), only when keys
# are installed in $F/ssh/authorized_keys and it isn't switched off in the web UI
# ($F/ssh/disabled; clockd also starts and stops it). Host keys are made on first start (-R).
# Retried with decast: the vendor sshd (eng build) may still hold port 22 at first.
ssh_up() {
    [ -s $F/ssh/authorized_keys ] && [ -x $F/bin/dropbearmulti ] && [ ! -e $F/ssh/disabled ] || return 0
    [ -f /tmp/dropbear.pid ] && kill -0 "$(cat /tmp/dropbear.pid)" 2>/dev/null && return 0
    $F/bin/dropbearmulti dropbear -R -s -g -D $F/ssh -P /tmp/dropbear.pid </dev/null >>$LOG 2>&1
}

# wifi_display.sh (/system/usr/bin, started for lenovo_alarm, which is stopped above):
# every 2 s it runs wpa_cli to keep /tmp/wifi_connected for that app's Wi-Fi icon;
# nothing of ours reads it. Stopped, not removed. To keep it running:
#   touch /data/local/clockd/use-wifi_display
stop_wifi_display() {
    [ -e $F/use-wifi_display ] && return 0
    for p in /proc/[0-9]*; do
        case "$(cat $p/cmdline 2>/dev/null | tr '\0' ' ')" in
        *" /usr/bin/wifi_display.sh"*|*" /system/usr/bin/wifi_display.sh"*) kill "${p#/proc/}" 2>/dev/null ;;
        esac
    done
}

decast() {
    cpu_governor
    stop_wifi_display
    for s in $UNWANTED; do stop $s; done
    for p in process_manager cast_shell update_engine crash_uploader; do
        kill $(pidof $p) 2>/dev/null
    done
    # in the background: without a lease udhcpc tries ~12 s before it forks, and
    # clockd (setup hotspot, face) shouldn't wait for that
    [ -n "$OWN_DHCP" ] && /sbin/busybox sh $F/bin/dhcp.sh start >>$LOG 2>&1 &
    ssh_up
}

decast
# init may still be bringing services up; repeat while the boot settles
( for i in 1 2 3 4 5 6 7 8 9 10 11 12; do sleep 10; decast; done ) &

# The microphones are clockd's (its own capture + apmd); the vendor capture
# service is the fallback only and clockd starts it when it needs it (the
# "Original" setting, or its own capture failing). Old way, running from boot
# until clockd takes over:  touch /data/local/clockd/use-iot_audio
if [ -e $F/use-iot_audio ]; then start iot_audio; else stop iot_audio; fi

# Your own additions: $F/user/boot.sh runs once per boot, as root, in the
# background (clockd doesn't wait for it; updates never touch $F/user).
# Skip it:  touch /data/local/clockd/user/disabled
if [ -f $F/user/boot.sh ] && [ ! -e $F/user/disabled ] && [ ! -e /tmp/clockd.user-boot ]; then
    touch /tmp/clockd.user-boot
    echo "$(date) start.sh: running user/boot.sh" >> $LOG
    ( cd $F/user && /sbin/busybox sh ./boot.sh </dev/null >>$LOG 2>&1
      echo "$(date) start.sh: user/boot.sh exited with $?" >> $LOG ) &
fi

# Updates (clockd -install, internal/release): the replaced files wait in
# update/prev and update/trial marks the new version until clockd has run a
# minute (it then removes the marker). If clockd keeps exiting before that, or
# the clock lost power in the middle of the swap (update/swapping), put the
# old files back.
B=/sbin/busybox
U=$F/update
rollback() {
    echo "$(date) start.sh: update $(cat $U/trial $U/swapping 2>/dev/null) failed ($1): rolling back" >> $LOG
    if [ -d $U/prev ]; then
        ( cd $U/prev && $B find . -type f ) | while read -r f; do
            f=${f#./}
            case $f in */*) $B mkdir -p "$F/${f%/*}" ;; esac
            $B mv -f "$U/prev/$f" "$F/$f"
        done
    fi
    # files the update added (the old version had none)
    [ -f $U/added ] && while read -r f; do
        [ -n "$f" ] && $B rm -f "$F/$f"
    done < $U/added
    # "from to date reason" (the web UI shows it)
    echo "$(cat $U/trial $U/swapping 2>/dev/null) $($B date +%Y-%m-%dT%H:%M) $1" > $U/failed
    $B rm -rf $U/prev $U/added $U/trial $U/swapping $U/tries
    sync
    short=0 # the old version gets its own tries
}
check_update() {
    if [ -f $U/swapping ]; then
        rollback "interrupted"
    elif [ -f $U/trial ]; then
        t=$(( $(cat $U/tries 2>/dev/null || echo 0) + 1 ))
        if [ $t -gt 3 ]; then
            rollback "clockd exited 3 times"
        else
            echo $t > $U/tries
        fi
    fi
}

# A clockd that keeps failing within a minute of starting (an update on trial
# rolls back first, above) would also take the buttons with it, and with them
# the 12 s back-to-stock hold: the only way out on a locked clock without ssh.
# After 5 such exits in a row (non-zero: a restart on purpose exits 0), clockd -rescue runs instead until the next power
# cycle: "Err" on the face and the hold, nothing else. user/boot.sh, if it ran
# this boot, is switched off (user/disabled) in case it was the cause.
rescue_mode() {
    echo "$(date) start.sh: clockd failed $short times within a minute of starting: rescue mode until the next power cycle" >> $LOG
    if [ -e /tmp/clockd.user-boot ] && [ ! -e $F/user/disabled ]; then
        touch $F/user/disabled
        echo "$(date) start.sh: user/boot.sh switched off (rm $F/user/disabled to switch it back on)" >> $LOG
    fi
    while true; do
        ( cd $F && exec ./clockd -rescue >> $LOG 2>&1 )
        echo "$(date) start.sh: clockd -rescue exited with $?" >> $LOG
        sleep 10
    done
}
uptime_s() { read -r up _ < /proc/uptime; echo ${up%.*}; }

# supervise clockd
n=0
short=0
while true; do
    check_update
    [ $short -ge 5 ] && rescue_mode
    [ -f $LOG ] && [ "$(stat -c %s $LOG 2>/dev/null || echo 0)" -gt 2000000 ] && mv $LOG $LOG.1
    # log in the clock's zone (clockd saves the one Home Assistant sends), not the firmware's
    T=$(/sbin/busybox sed -n 's/.*"timezone": *"\([^"]*\)".*/\1/p' $F/state.json 2>/dev/null)
    [ -n "$T" ] && export TZ="$T"
    echo "$(date) start.sh: starting clockd (run $n)" >> $LOG
    ( cd $F && exec ./clockd >> $LOG 2>&1 ) &
    pid=$!
    # an update on trial that hangs instead of exiting: clockd confirms a
    # healthy update after a minute and exits by itself if its health check
    # still fails after four; a frozen process gets killed here after five
    # (each kill counts as a try, the third rolls back)
    if [ -f $U/trial ]; then
        ( sleep 300
          if [ -f $U/trial ] && kill -0 $pid 2>/dev/null; then
              echo "$(date) start.sh: update $(cat $U/trial) still not confirmed after 5 min: killing clockd" >> $LOG
              kill -9 $pid
          fi ) &
    fi
    t0=$(uptime_s)
    wait $pid
    rc=$?
    echo "$(date) start.sh: clockd exited with $rc" >> $LOG
    # a restart on purpose (button, restart.sh, new time zone) exits 0
    if [ $rc -ne 0 ] && [ $(( $(uptime_s) - t0 )) -lt 60 ]; then short=$((short + 1)); else short=0; fi
    n=$((n + 1))
    sleep 3
done
