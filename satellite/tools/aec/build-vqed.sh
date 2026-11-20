#!/bin/sh
# Build vqed (LocalVQE pipe filter) as a static aarch64 binary with zig.
#
#	tools/aec/build-vqed.sh <workdir>
#
# Clones LocalVQE (Apache-2.0) at a fixed commit into <workdir>, shortens the
# DAF echo filter to 32 partitions (0.256 s; 128 = 1.024 s is too slow for the
# A53 and longer than the clock's echo path needs), builds the static libs and
# links vqed. Output: <workdir>/vqed-arm64. Model:
# https://huggingface.co/LocalAI-io/LocalVQE/resolve/main/localvqe-v1.4-aec-2.7K-f32.gguf
set -eu
W=$(realpath "$1")
HERE=$(dirname "$(realpath "$0")")
ZIG=${ZIG:-zig}
COMMIT=7f61cd0
mkdir -p "$W" && cd "$W"
[ -d LocalVQE ] || git clone -q https://github.com/localai-org/LocalVQE
cd LocalVQE && git checkout -q $COMMIT && git submodule update -q --init --recursive --depth 1 && cd ..
sed -i 's|static constexpr int N = 128; .*|static constexpr int N = 32;           // partitions (0.256 s span; upstream 128)|' LocalVQE/ggml/daf_frontend.h
grep -q 'int N = 32;' LocalVQE/ggml/daf_frontend.h
mkdir -p zbin
printf '#!/bin/sh\nexec %s cc -target aarch64-linux-musl -mcpu=cortex_a53 "$@"\n' "$ZIG" > zbin/cc
printf '#!/bin/sh\nexec %s c++ -target aarch64-linux-musl -mcpu=cortex_a53 "$@"\n' "$ZIG" > zbin/c++
chmod +x zbin/cc zbin/c++
sed "s|ZBIN|$W/zbin|" "$HERE/zig-aarch64.cmake" > tc.cmake
cmake -S LocalVQE/ggml -B barm -G Ninja -DCMAKE_BUILD_TYPE=Release -DCMAKE_TOOLCHAIN_FILE="$W/tc.cmake" \
	-DBUILD_SHARED_LIBS=OFF -DGGML_OPENMP=OFF >/dev/null
cmake --build barm --target dvqe_graph ggml ggml-cpu ggml-base >/dev/null
L=LocalVQE/ggml
zbin/c++ -O2 -s -std=c++17 -static -DLOCALVQE_HAS_GTCRN -I$L -I$L/vendor/ggml/include -I$L/gtcrn \
	"$HERE/vqed.cpp" $L/localvqe_api.cpp -Wl,--start-group barm/libdvqe_graph.a barm/libdvqe_gtcrn.a \
	barm/libdvqe_common.a barm/vendor/ggml/src/libggml.a barm/vendor/ggml/src/libggml-cpu.a \
	barm/vendor/ggml/src/libggml-base.a -Wl,--end-group -lpthread -o vqed-arm64
ls -la vqed-arm64
