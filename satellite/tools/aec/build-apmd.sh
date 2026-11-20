#!/bin/sh
# Build apmd (WebRTC audio processing pipe filter) as a static aarch64 binary with zig.
#
#	tools/aec/build-apmd.sh <workdir>
#	tools/aec/build-apmd.sh --host <workdir>   for this machine (cmd/micbench, the
#	                                           offline bench): <workdir>/webrtc-audio-processing/bhost/examples/apmd
#	                                           needs meson, ninja and a C++ compiler; no zig
#
# Clones freedesktop webrtc-audio-processing (BSD-3), cross-builds it with meson
# (abseil as a subproject) and links tools/aec/apmd.cpp (and apmtool.cpp for the
# workstation tests, see apmsweep.sh). Output: <workdir>/webrtc-audio-processing/barm/examples/apmd
set -eu
HOST=
[ "${1:-}" = --host ] && { HOST=1; shift; }
[ $# = 1 ] || { echo "usage: $0 [--host] <workdir>" >&2; exit 2; }
mkdir -p "$1"
W=$(realpath "$1"); HERE=$(dirname "$(realpath "$0")"); ZIG=${ZIG:-zig}
mkdir -p "$W/zbin" && cd "$W"
for t in cc c++; do printf '#!/bin/sh\nexec %s %s -target aarch64-linux-musl -mcpu=cortex_a53 "$@"\n' "$ZIG" $t > zbin/$t; done
for t in ar ranlib; do printf '#!/bin/sh\nexec %s %s "$@"\n' "$ZIG" $t > zbin/$t; done
chmod +x zbin/*
sed "s|ZBIN|$W/zbin|g" "$HERE/webrtc-cross-arm64.ini" > cross-arm64.ini
[ -d webrtc-audio-processing ] || git clone -q https://gitlab.freedesktop.org/pulseaudio/webrtc-audio-processing.git
git -C webrtc-audio-processing checkout -q d0569cf  # tested 2026-11-03
cd webrtc-audio-processing
cp "$HERE/apmd.cpp" "$HERE/apmtool.cpp" examples/
grep -q "'apmd'" examples/meson.build || cat >> examples/meson.build <<'M'
executable('apmtool', 'apmtool.cpp', install: false, include_directories: top_incdir, dependencies: [audio_processing_dep, absl_dep])
executable('apmd', 'apmd.cpp', install: false, include_directories: top_incdir, dependencies: [audio_processing_dep, absl_dep], link_args: meson.is_cross_build() ? ['-static', '-s'] : [])
M
if [ -n "$HOST" ]; then
	[ -d bhost ] || meson setup bhost --buildtype=release --default-library=static -Dforce_fallback_for=abseil-cpp >/dev/null
	ninja -C bhost examples/apmd examples/apmtool >/dev/null
	ls -la bhost/examples/apmd
	exit 0
fi
[ -d barm ] || meson setup barm --cross-file ../cross-arm64.ini --buildtype=release --default-library=static -Dforce_fallback_for=abseil-cpp >/dev/null
ninja -C barm examples/apmd >/dev/null
ls -la barm/examples/apmd
