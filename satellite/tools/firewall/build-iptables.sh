#!/bin/sh
# Build a static aarch64 iptables (legacy, IPv4) for the clock with zig.
# The clock's kernel has iptables (filter, conntrack/state, tcp/udp) but no
# userspace tool; the setup hotspot's firewall (device/setup-ap.sh) needs it.
# No multiport in the kernel: one rule per port.
#
#	tools/firewall/build-iptables.sh <workdir>     -> build/bin/iptables
#
# ZIG (default: zig on PATH) must be 0.12+.
set -e
W=${1:?usage: $0 <workdir>}
V=1.8.10
ZIG=${ZIG:-zig}
OUT=$(cd "$(dirname "$0")/../.." && pwd)/build/bin
mkdir -p "$W" "$OUT"
cd "$W"
[ -d iptables-$V ] || { curl -sSfLO https://www.netfilter.org/projects/iptables/files/iptables-$V.tar.xz && tar xf iptables-$V.tar.xz; }
for t in cc ar ranlib; do
	case $t in cc) a='cc -target aarch64-linux-musl -mcpu=cortex_a53' ;; *) a=$t ;; esac
	printf '#!/bin/sh\nexec %s %s "$@"\n' "$ZIG" "$a" > z$t
	chmod +x z$t
done
cd iptables-$V
CC=$W/zcc AR=$W/zar RANLIB=$W/zranlib ./configure --host=aarch64-linux-musl \
	--disable-nftables --enable-static --disable-shared --disable-ipv6 --without-kernel \
	--disable-libipq --disable-bpf-compiler --disable-nfsynproxy --disable-connlabel LDFLAGS=-static >/dev/null
make -j"$(nproc)" >/dev/null
cp iptables/xtables-legacy-multi "$OUT/iptables"
echo "built $OUT/iptables (run on the clock with XTABLES_LOCKFILE=/tmp/xtables.lock)"
