#!/sbin/busybox sh
# setup-ap.sh: the first-time setup hotspot (clockd starts and stops it).
#
#	setup-ap.sh start <ssid>    access point on uap0 (192.168.255.249/29) and
#	                            DHCP (clockd answers DNS: phones open the setup page)
#	setup-ap.sh stop
#	setup-ap.sh status
#
# Open, unless /data/local/clockd/hotspot.key holds a WPA2 key (64 hex digits,
# derived from the hotspot password and name by install-clock.sh
# --hotspot-password; the password itself is not on the clock). With a key the
# hotspot never starts open: a bad key file, or the net_mgr route (which only
# does open hotspots), is an error.
#
# The access point is the vendor /bin/hostapd, started here the way the vendor
# net_mgr does it (net_mgr itself is stopped by start.sh). That hostapd drops to
# the wifi user before it reads its config, so the config and its control
# socket live under /tmp (not /data/local, which is root-only: hostapd would
# exit silently). uap0 gets 192.168.255.249/29, our busybox udhcpd hands out
# addresses and clockd answers every DNS name with the clock (captive portal).
# The vendor dnsmasq (2.51) did both before v0.103.0; it is the old route with
# the flag file /data/local/clockd/use-dnsmasq. uap0 is the Broadcom driver's
# second interface (init.rc), next to wlan0.
# Old route, kept as a fallback: with /data/local/clockd/use-net_mgr present,
# net_mgr runs and the hotspot goes through its socket ("hotspot" commands,
# sent with clockd -netmgr); net_mgr then also starts a DHCP-only dnsmasq,
# which is replaced by ours. Note: net_mgr syncs the time over HTTP with
# clients1.google.com whenever it starts.
#
# Anyone in range may be on it (open, or the password got around), so the
# clock is shielded on uap0: only DHCP, DNS and the
# setup page (port 80) are reachable from it (adb 5555 is unauthenticated root
# and must never be), IPv6 is off there and nothing is forwarded. Without
# iptables the hotspot does not start.
B=/sbin/busybox
F=/data/local/clockd
S=$F/setup-ap
IPT=$F/bin/iptables
UDHCPD="$F/bin/busybox udhcpd" # forks into the background (pidfile in its config)
export XTABLES_LOCKFILE=/tmp/xtables.lock
IP=192.168.255.249
A=/tmp/clockd-ap # hostapd's config and control socket (readable by the wifi user)
mkdir -p $S
[ -e $F/use-net_mgr ] && NETMGR=1 || NETMGR=

# own_up / own_down: hostapd without net_mgr
own_up() {
	own_down
	$B mkdir -p $A/ctrl
	$B chmod 755 $A
	$B chown 1008:1008 $A/ctrl # the wifi user (busybox knows no Android names)
	$B chmod 770 $A/ctrl
	# like net_mgr: share the client's channel when it is on 2.4 GHz, else 6
	ch=$(iw dev wlan0 info 2>/dev/null | $B sed -n 's/.*channel \([0-9]*\).*/\1/p')
	case "$ch" in [1-9]|1[0-4]) ;; *) ch=6 ;; esac
	# printf, not echo: the SSID is ours (LenovoClock-XXXX) but keep it literal
	# the config can hold the key: only for the wifi user, who reads it
	: > $A/hostapd.conf
	$B chown 1008:1008 $A/hostapd.conf
	$B chmod 600 $A/hostapd.conf
	printf 'interface=uap0\ndriver=nl80211\nctrl_interface=%s/ctrl\nchannel=%s\nhw_mode=g\nieee80211n=1\nssid=%s\nignore_broadcast_ssid=0\n' \
		$A "$ch" "$1" >> $A/hostapd.conf
	if [ -n "$KEY" ]; then
		printf 'auth_algs=1\nwpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nwpa_psk=%s\n' "$KEY" >> $A/hostapd.conf
	fi
	ifconfig uap0 up || return 1
	$B setsid /bin/hostapd -e $A/entropy.bin $A/hostapd.conf </dev/null >$A/hostapd.log 2>&1 &
	echo $! > $A/hostapd.pid
	n=0
	until iw dev uap0 info 2>/dev/null | $B grep -q "type AP"; do
		n=$((n + 1))
		[ $n -gt 20 ] && { echo "hostapd did not start the access point (see logcat: hostapd)"; return 1; }
		$B sleep 0.5
	done
	ifconfig uap0 $IP netmask 255.255.255.248 up
}

own_down() {
	if [ -f $A/hostapd.pid ]; then
		kill "$($B cat $A/hostapd.pid)" 2>/dev/null
		rm -f $A/hostapd.pid
		$B sleep 0.5
		ifconfig uap0 down 2>/dev/null
	fi
	rm -rf $A
}

own_running() { [ -f $A/hostapd.pid ] && kill -0 "$($B cat $A/hostapd.pid)" 2>/dev/null; }

nm() { $F/clockd -netmgr "$*" 2>&1; }

fw_on() {
	fw_off
	$IPT -N CLOCKSETUP || return 1
	$IPT -A CLOCKSETUP -p udp --dport 67 -j ACCEPT &&
	$IPT -A CLOCKSETUP -p udp --dport 53 -j ACCEPT &&
	$IPT -A CLOCKSETUP -p tcp --dport 53 -j ACCEPT &&
	$IPT -A CLOCKSETUP -p tcp --dport 80 -j ACCEPT &&
	$IPT -A CLOCKSETUP -p icmp -j ACCEPT &&
	$IPT -A CLOCKSETUP -j DROP &&
	$IPT -I INPUT 1 -i uap0 -j CLOCKSETUP &&
	$IPT -I FORWARD 1 -i uap0 -j DROP
}

fw_off() {
	[ -x $IPT ] || return 0
	while $IPT -D INPUT -i uap0 -j CLOCKSETUP 2>/dev/null; do :; done
	while $IPT -D FORWARD -i uap0 -j DROP 2>/dev/null; do :; done
	$IPT -F CLOCKSETUP 2>/dev/null
	$IPT -X CLOCKSETUP 2>/dev/null
	return 0
}

stop() {
	if [ -f $S/dnsmasq.pid ]; then
		kill "$($B cat $S/dnsmasq.pid)" 2>/dev/null
		rm -f $S/dnsmasq.pid
	fi
	if [ -f $S/udhcpd.pid ]; then
		kill "$($B cat $S/udhcpd.pid)" 2>/dev/null
		rm -f $S/udhcpd.pid
	fi
	if [ -n "$NETMGR" ]; then
		case "$(nm hotspot status)" in *Started*) nm hotspot stop >/dev/null ;; esac
	else
		own_down
	fi
	fw_off
}

case "$1" in
start)
	SSID="$2"
	[ -n "$SSID" ] || { echo "usage: $0 start <ssid>"; exit 2; }
	stop
	KEY=
	if [ -e $F/hotspot.key ]; then
		KEY=$($B tr -d ' \r\n' < $F/hotspot.key)
		case "$KEY" in *[!0-9a-fA-F]*) KEY=bad ;; esac
		[ ${#KEY} -eq 64 ] || { echo "hotspot.key is not a 64-digit key: not starting the hotspot open (set it again with install-clock.sh --hotspot-password)"; exit 1; }
		[ -z "$NETMGR" ] || { echo "the net_mgr route (use-net_mgr) cannot give the hotspot a password: remove hotspot.key or use-net_mgr"; exit 1; }
	fi
	[ -x $IPT ] || { echo "missing $IPT: not starting a hotspot without a firewall"; exit 1; }
	fw_on || { echo "firewall rules failed"; stop; exit 1; }
	if [ -n "$NETMGR" ]; then
		start net_mgr
		r=$(nm hotspot ssid "$SSID") || { echo "net_mgr: $r"; stop; exit 1; }
		r=$(nm hotspot start) || { echo "net_mgr hotspot start: $r"; stop; exit 1; }
		# wait for uap0 to be an AP with net_mgr's address
		n=0
		until ifconfig uap0 2>/dev/null | $B grep -q "inet addr:$IP"; do
			n=$((n + 1))
			[ $n -gt 20 ] && { echo "uap0 did not come up as $IP (net_mgr: $r)"; stop; exit 1; }
			$B sleep 0.5
		done
	else
		own_up "$SSID" || { stop; exit 1; }
	fi
	[ -n "$KEY" ] && KIND="with a password" || KIND=open
	echo 1 > /proc/sys/net/ipv6/conf/uap0/disable_ipv6 2>/dev/null
	# net_mgr's dnsmasq (fallback route) does DHCP only (port=0): replace it
	for p in $($B pidof dnsmasq); do kill -9 $p 2>/dev/null; done
	if [ -e $F/use-dnsmasq ]; then
		# old route: the vendor dnsmasq 2.51 does DHCP and the catch-all DNS
		$B sleep 0.5
		# -C /dev/null: not the vendor's /etc/dnsmasq.conf (no-daemon, port=0 = no DNS, user=chrome)
		# --user: there is no "nobody"; the vendor runs it as chrome:inet
		dnsmasq -C /dev/null --user=chrome --group=inet --interface=uap0 --bind-interfaces --except-interface=lo --no-resolv --no-hosts \
			--dhcp-range=192.168.255.250,192.168.255.254,255.255.255.248,15m --dhcp-option=3,$IP --dhcp-option=6,$IP \
			--address=/#/$IP --dhcp-leasefile=$S/leases --pid-file=$S/dnsmasq.pid \
			</dev/null >$S/dnsmasq.log 2>&1 || { cat $S/dnsmasq.log; stop; exit 1; } # its own output: an inherited pipe kept clockd waiting
	else
		# DHCP: our busybox udhcpd; DNS: clockd itself (every name is the clock)
		printf 'interface uap0\nstart 192.168.255.250\nend 192.168.255.254\nmax_leases 5\nauto_time 0\nlease_file %s/udhcpd.leases\npidfile %s/udhcpd.pid\nopt subnet 255.255.255.248\nopt router %s\nopt dns %s\nopt lease 900\n' \
			$S $S $IP $IP > $S/udhcpd.conf
		: > $S/udhcpd.leases
		$UDHCPD $S/udhcpd.conf </dev/null >$S/udhcpd.log 2>&1 || { cat $S/udhcpd.log; stop; exit 1; }
		$B sleep 0.3
		kill -0 "$($B cat $S/udhcpd.pid 2>/dev/null)" 2>/dev/null || { echo "udhcpd did not start: $($B cat $S/udhcpd.log)"; stop; exit 1; }
	fi
	CH=$(iw dev uap0 info 2>/dev/null | $B sed -n 's/.*channel \([0-9]*\).*/\1/p')
	echo "setup hotspot \"$SSID\" on channel ${CH:-?}, $IP, $KIND"
	;;
stop)
	stop
	echo "setup hotspot stopped"
	;;
status)
	if [ -n "$NETMGR" ]; then
		case "$(nm hotspot status)" in *Started*) up=1 ;; *) up= ;; esac
	else
		own_running && up=1 || up=
	fi
	if [ -n "$up" ]; then
		echo "running"
		if [ -e $F/use-dnsmasq ]; then
			cat $S/leases 2>/dev/null
		else
			$F/bin/busybox dumpleases -f $S/udhcpd.leases 2>/dev/null
		fi
	else
		echo "stopped"
	fi
	;;
*)
	echo "usage: $0 start <ssid> | stop | status"
	exit 2
	;;
esac
