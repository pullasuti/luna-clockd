#!/bin/sh
# apmsweep.sh <raw.wav (16 kHz s16, 1-2 ch)> <vendor.wav>: score APM variants against the vendor stream
S=$(cd "$(dirname "$0")/../.." && pwd)
A=${APMTOOL:?set APMTOOL to the PC apmtool (tools/aec/build-apmd.sh --host)}
WW=$S/build/wwfile
M=$S/models
T=${TMPDIR:-/tmp}/apmsweep; mkdir -p $T
score() { # name file
	$WW $M "$2" 2>/dev/null | python3 -c "
import sys, re
w = s = []
for l in sys.stdin:
    v = [float(x) for x in re.findall(r':([0-9.]+)', l)]
    if 'okay_nabu' in l: w = v
    if 'stop ' in l: s = v
f = lambda xs, t: sum(1 for x in xs if x >= t)
print(f'%-34s nabu >=.5 {f(w,.5):2d}  >=.8 {f(w,.8):2d}  >=.9 {f(w,.9):2d} | stop >=.5 {f(s,.5)}' % '$1')"
}
score "vendor (iot_audio_capture)" "$2"
run() { name=$1; shift; $A "$@" "$RAW" $T/o.wav && score "$name" $T/o.wav; }
RAW=$1
run "raw, no processing" --ch 0
run "hpf" --hpf
run "ns3" --ns 3
run "hpf ns3 agc1-digital" --hpf --ns 3 --agc1 digital
run "hpf ns3 agc2" --hpf --ns 3 --agc2
run "hpf ns2 agc2" --hpf --ns 2 --agc2
run "hpf ns1 agc2" --hpf --ns 1 --agc2
run "hpf agc2" --hpf --agc2
run "hpf ns3 agc2, ch0 only" --hpf --ns 3 --agc2 --ch 0
run "hpf ns3 agc2 +10 dB" --hpf --ns 3 --agc2 --pre 10
