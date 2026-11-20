#!/bin/bash
# Dump all MTD partitions from the Lenovo Smart Clock via chunked dd -> /tmp (tmpfs) -> adb pull.
# Read-only on flash (uses /dev/mtd/mtdNro). Each chunk verified by sha256.
#
#	dump-partitions.sh [SERIAL] [OUTDIR]     (default: the only adb device, ./partitions)
#
# The layout (names, sizes) is the CD-4N341Y's; the script checks /proc/mtd matches.
set -u
SERIAL=${1:-${ADB_SERIAL:-}}
OUT=$(realpath -m "${2:-partitions}")
adb() { command adb ${SERIAL:+-s "$SERIAL"} "$@"; }
want=$'mtd0: 00400000 00040000 "bootloader"\nmtd1: 00800000 00040000 "tpl"\nmtd2: 00100000 00040000 "misc"\nmtd3: 01000000 00040000 "boot_a"\nmtd4: 01000000 00040000 "boot_b"\nmtd5: 0a000000 00040000 "system_a"\nmtd6: 0a000000 00040000 "system_b"\nmtd7: 01000000 00040000 "factory"\nmtd8: 07700000 00040000 "data"'
have=$(adb shell cat /proc/mtd | tr -d '\r' | sed 1d)
[ "$have" = "$want" ] || { echo "unexpected /proc/mtd:"; echo "$have"; exit 1; }
BS=262144; CNT=32            # 8 MiB chunks
BB=/sbin/busybox
mkdir -p "$OUT"; cd "$OUT"
names=(bootloader tpl misc boot_a boot_b system_a system_b factory data)
sizes=(0x400000 0x800000 0x100000 0x1000000 0x1000000 0xa000000 0xa000000 0x1000000 0x7700000)
for i in "${!names[@]}"; do
  f="mtd${i}_${names[$i]}.bin"; size=$((sizes[$i])); : > "$f"
  nchunks=$(( (size + BS*CNT - 1) / (BS*CNT) ))
  for ((c=0; c<nchunks; c++)); do
    for try in 1 2 3; do
      dev=$(adb shell "$BB dd if=/dev/mtd/mtd${i}ro of=/tmp/chunk bs=$BS count=$CNT skip=$((c*CNT)) conv=noerror,sync 2>/dev/null; $BB sha256sum /tmp/chunk" | tr -d '\r' | cut -d' ' -f1)
      adb pull /tmp/chunk chunk.tmp >/dev/null 2>&1
      host=$(sha256sum chunk.tmp | cut -d' ' -f1)
      [ "$dev" = "$host" ] && break
      echo "  retry $f chunk $c ($try)"
    done
    [ "$dev" = "$host" ] || { echo "FAIL $f chunk $c"; exit 1; }
    cat chunk.tmp >> "$f"
  done
  adb shell "rm -f /tmp/chunk"; rm -f chunk.tmp
  echo "$f $(stat -c%s "$f") bytes OK"
done
sha256sum mtd*.bin > SHA256SUMS
echo DONE
