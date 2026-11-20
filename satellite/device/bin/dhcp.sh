#!/sbin/busybox sh
# dhcp.sh - network address for wlan0 with our busybox udhcpc, in place of the
# vendor dhcpcd 6.8.2 (start.sh stops that one; touch /data/local/clockd/use-dhcpcd
# to go back). One script, two callers:
#
#	dhcp.sh start                 start udhcpc
#	dhcp.sh deconfig|bound|renew|leasefail|nak   (udhcpc, with its env vars)
#
# After a new Wi-Fi association (another network or access point, or back after a
# drop) the address has to be asked for again: clockd does that, on wpa_supplicant's
# CTRL-EVENT-CONNECTED it sends udhcpc SIGUSR1 (cmd/clockd/wifiwatch.go). Same
# network: a quick renew; another network: the renew goes unanswered and udhcpc
# starts over. Every answer rewrites /tmp/dhcp-lease, which is how clockd knows the
# network it just joined has answered (the address itself may be the same as before).
#
# Differences from the vendor dhcpcd: the router learns the clock's own name
# (state.json device_name, e.g. lenovo-clock-1a2b, instead of "lenovo"/"audiocast"),
# and /etc/resolv.conf (-> /tmp/resolv.conf) lists only the DNS servers the
# router hands out - the vendor hooks put Google's 8.8.8.8 first.
F=/data/local/clockd
BB=$F/bin/busybox
IF=wlan0
PID=/tmp/udhcpc.pid
RESOLV=/tmp/resolv.conf
EV=$1
# udhcpc runs in the background with its output closed: our lines go to clockd's log
log() { echo "$($BB date) dhcp.sh: $*" >> /tmp/clockd.log; }

case "$1" in
start)
	# started only when it isn't running (start.sh calls this repeatedly)
	if ! { [ -f $PID ] && kill -0 "$($BB cat $PID)" 2>/dev/null; }; then
		N=$($BB sed -n 's/.*"device_name": *"\([^"]*\)".*/\1/p' $F/state.json 2>/dev/null)
		N=${N:-lenovo-clock}
		# keep the address the vendor dhcpcd had (it is killed, not released): ask for it again
		CUR=$($BB ip -4 addr show dev $IF 2>/dev/null | $BB sed -n 's/.*inet \([0-9.]*\)\/.*/\1/p' | $BB head -n 1)
		# udhcpc runs "deconfig" when it starts: keep the address through that one
		[ -n "$CUR" ] && : > /tmp/dhcp-keep
		# -b: go to the background when there is no lease yet (no Wi-Fi at boot / setup mode)
		$BB udhcpc -i $IF -b -p $PID -s $F/bin/dhcp.sh -x hostname:"$N" -F "$N" \
			-t 4 -T 3 -A 5 ${CUR:+-r $CUR} -O dns -O router -O subnet
	fi
	;;
deconfig)
	# the first deconfig after "start" keeps the address taken over from dhcpcd
	[ -f /tmp/dhcp-keep ] && { rm -f /tmp/dhcp-keep; exit 0; }
	$BB ip -4 addr flush dev $IF
	$BB ip link set $IF up
	log "deconfig: address removed"
	;;
bound|renew)
	rm -f /tmp/dhcp-keep
	CUR=$($BB ip -4 addr show dev $IF | $BB sed -n 's/.*inet \([0-9.]*\/[0-9]*\).*/\1/p' | $BB head -n 1)
	NEW=$ip/$($BB ipcalc -p "$ip" "$subnet" | $BB cut -d= -f2)
	if [ "$CUR" != "$NEW" ]; then
		$BB ip -4 addr flush dev $IF
		$BB ip addr add "$NEW" broadcast "${broadcast:-+}" dev $IF
	fi
	# exactly one default route, via this lease's router (dhcpcd's metric-305 one goes)
	set -- $router
	if [ -n "$1" ] && [ "$($BB ip route show default | $BB tr -s ' ' | $BB sed 's/ $//')" != "default via $1 dev $IF" ]; then
		while $BB ip route del default 2>/dev/null; do :; done
		$BB ip route add default via "$1" dev $IF
	fi
	{
		[ -n "$domain" ] && echo "search $domain"
		for d in $dns; do echo "nameserver $d"; done
		echo "options timeout:2 attempts:3"
	} > $RESOLV.new
	if $BB cmp -s $RESOLV.new $RESOLV; then rm -f $RESOLV.new; else $BB mv -f $RESOLV.new $RESOLV; chmod 644 $RESOLV; fi
	echo "$EV $ip" > /tmp/dhcp-lease
	log "$EV $ip/$subnet router ${router:-none} dns ${dns:-none}"
	;;
leasefail|nak)
	log "$EV"
	;;
esac
exit 0
