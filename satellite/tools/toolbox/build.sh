#!/bin/sh
# Build static aarch64 tools for the clock with zig (musl), from pinned upstream
# sources (checked against the sha256 below).
#
#	tools/toolbox/build.sh <workdir> [busybox] [dropbear] [strace] [tcpdump]
#	                                             (default: all four)
#
#	busybox, dropbearmulti -> build/bin/ (part of the clockd package)
#	strace, tcpdump   -> build/tools/bin (optional tools, install-clock.sh --tools)
#
# ZIG (default: zig on PATH) 0.16+. Needs make, a host cc (gcc), curl, perl (busybox).
# Licences: busybox, strace (LGPL-2.1+ / GPL-2.0+ for parts) and the GPL parts are
# built unmodified except where noted (patches/); dropbear (MIT), tcpdump +
# libpcap (BSD). Sources: the URLs below, exactly these versions.
set -eu
W=$(mkdir -p "${1:?usage: $0 <workdir> [tool...]}" && cd "$1" && pwd); shift
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(cd "$HERE/../.." && pwd)/build
ZIG=${ZIG:-zig}
TOOLS=${*:-busybox dropbear strace tcpdump}
mkdir -p "$OUT/bin" "$OUT/tools/bin" "$W/zbin"
J=$(nproc)

# cross compiler wrappers (generic armv8-a: the clock's A113X is a Cortex-A53; no -mcpu,
# zig's clang driver rejects it on some inputs, e.g. strace's preprocessed sources)
printf '#!/bin/sh\nexec %s cc -target aarch64-linux-musl "$@"\n' "$ZIG" > "$W/zbin/cc"
for t in ar ranlib; do printf '#!/bin/sh\nexec %s %s "$@"\n' "$ZIG" $t > "$W/zbin/$t"; done
# strip: zig's objcopy can't strip these ("unimplemented"); binaries are linked with -s
# instead, and strip only copies for build systems that call it (busybox: strip IN -o OUT).
cat > "$W/zbin/strip" <<'EOF'
#!/bin/sh
in= out=
while [ $# -gt 0 ]; do
	case $1 in -o) out=$2; shift 2 ;; -*) shift ;; *) in=$1; shift ;; esac
done
[ -z "$out" ] || cp -f "$in" "$out"
EOF
chmod +x "$W/zbin/"*
ZCC=$W/zbin/cc

# fetch NAME URL SHA256: download once into the workdir and verify
fetch() {
	f=$W/$(basename "$2")
	[ -f "$f" ] || curl -sSfL -o "$f" "$2"
	echo "$3  $f" | sha256sum -c --quiet || { echo "checksum mismatch: $f"; rm -f "$f"; exit 1; }
	rm -rf "$W/$1"; mkdir -p "$W/$1"
	tar xf "$f" -C "$W/$1" --strip-components=1
}

build_busybox() {
	V=1.38.0
	fetch busybox https://busybox.net/downloads/busybox-$V.tar.bz2 "$BUSYBOX_SHA"
	cd "$W/busybox"
	M="make -s HOSTCC=gcc CC=$ZCC AR=$W/zbin/ar STRIP=$W/zbin/strip LDFLAGS=-s"
	$M defconfig >/dev/null
	# static; off: x86-only SHA acceleration, tc (needs newer kernel headers),
	# and network servers we never want on the clock (telnetd, httpd, ftpd, tftpd, inetd)
	for o in STATIC; do sed -i "s/^# CONFIG_$o is not set/CONFIG_$o=y/" .config; done
	for o in SHA1_HWACCEL SHA256_HWACCEL TC TELNETD HTTPD FTPD TFTPD INETD FEATURE_INETD_RPC; do
		sed -i "s/^CONFIG_$o=y/# CONFIG_$o is not set/" .config
	done
	yes '' | $M oldconfig >/dev/null
	$M -j"$J" busybox >/dev/null
	cp busybox "$OUT/bin/busybox"
	echo "built $OUT/bin/busybox ($V)"
}

build_dropbear() {
	V=2026.94
	fetch dropbear https://matt.ucc.asn.au/dropbear/releases/dropbear-$V.tar.bz2 "$DROPBEAR_SHA"
	cd "$W/dropbear"
	# our options; also the built-in root entry (the clocks have no /etc/passwd)
	cp "$HERE/patches/dropbear-localoptions.h" localoptions.h
	CC=$ZCC AR=$W/zbin/ar RANLIB=$W/zbin/ranlib ./configure --host=aarch64-linux-musl \
		--enable-static --disable-zlib --disable-lastlog --disable-utmp --disable-utmpx \
		--disable-wtmp --disable-wtmpx --disable-pututline --disable-pututxline \
		--disable-syslog LDFLAGS="-static -s" >/dev/null
	make -s -j"$J" PROGRAMS="dropbear dropbearkey" MULTI=1 SCPPROGRESS=0 >/dev/null
	cp dropbearmulti "$OUT/bin/dropbearmulti"
	echo "built $OUT/bin/dropbearmulti ($V; 'dropbearmulti dropbear ...', 'dropbearmulti dropbearkey ...')"
}

build_strace() {
	V=6.19 # matches zig 0.16's kernel headers (Linux 6.19); 7.x needs newer ones
	fetch strace https://github.com/strace/strace/releases/download/v$V/strace-$V.tar.xz "$STRACE_SHA"
	cd "$W/strace"
	CC=$ZCC AR=$W/zbin/ar RANLIB=$W/zbin/ranlib ./configure --host=aarch64-linux-musl \
		--enable-mpers=no --disable-gcc-Werror LDFLAGS="-static -s" >/dev/null
	make -s -j"$J" >/dev/null
	cp src/strace "$OUT/tools/bin/strace"
	echo "built $OUT/tools/bin/strace ($V)"
}

build_tcpdump() {
	PV=1.10.7 TV=4.99.7
	fetch libpcap https://www.tcpdump.org/release/libpcap-$PV.tar.xz "$LIBPCAP_SHA"
	cd "$W/libpcap"
	CC=$ZCC AR=$W/zbin/ar RANLIB=$W/zbin/ranlib ./configure --host=aarch64-linux-musl \
		--disable-shared --without-libnl --disable-usb --disable-netmap --disable-bluetooth \
		--disable-dbus --disable-rdma --without-dpdk >/dev/null
	make -s -j"$J" libpcap.a >/dev/null
	fetch tcpdump https://www.tcpdump.org/release/tcpdump-$TV.tar.xz "$TCPDUMP_SHA"
	cd "$W/tcpdump"
	CC=$ZCC AR=$W/zbin/ar RANLIB=$W/zbin/ranlib ./configure --host=aarch64-linux-musl \
		--without-crypto --without-smi --without-cap-ng LDFLAGS="-static -s" \
		--with-pcap=linux CPPFLAGS="-I$W/libpcap" LIBS="$W/libpcap/libpcap.a" \
		ac_cv_lib_pcap_pcap_open_live=yes >/dev/null
	make -s -j"$J" >/dev/null
	cp tcpdump "$OUT/tools/bin/tcpdump"
	echo "built $OUT/tools/bin/tcpdump ($TV, libpcap $PV)"
}

. "$HERE/sources.sha256"
for t in $TOOLS; do
	case $t in
	busybox|dropbear|strace|tcpdump) (build_$t) ;;
	*) echo "unknown tool $t"; exit 2 ;;
	esac
done
