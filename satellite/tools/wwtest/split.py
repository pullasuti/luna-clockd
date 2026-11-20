#!/usr/bin/env python3
"""Split a test capture (run.py --record, or GET /api/capture) into the files
the offline tools take:

    split.py capture.wav [outdir]   ->  mic.wav   canceller input (mono, after its pre-gain)
                                        mics.wav  the two mics (stereo, same gain)
                                        ref.wav   speaker reference (mono)
                                        out.wav   what the wake word models got

Replay with another setting:  apmtool --pre 0 [options] mic.wav new.wav ref.wav
(captures from v0.49.0 have mic before the pre-gain: use --pre 40 for those)
Score:                         wwfile <models dir> out.wav   (or new.wav)
"""
import os
import sys
import wave

src = sys.argv[1]
out = sys.argv[2] if len(sys.argv) > 2 else os.path.splitext(src)[0]
os.makedirs(out, exist_ok=True)
with wave.open(src) as w:
    ch, rate = w.getnchannels(), w.getframerate()
    if ch != 5 or w.getsampwidth() != 2:
        raise SystemExit(f'{src}: expected 5 channels of 16-bit audio, got {ch}')
    data = w.readframes(w.getnframes())
frames = len(data) // 10
for name, chans in (('mic', [0]), ('mics', [1, 2]), ('ref', [3]), ('out', [4])):
    b = bytearray(frames * 2 * len(chans))
    for j, c in enumerate(chans):
        for i in range(frames):
            o = i * 10 + 2 * c
            p = (i * len(chans) + j) * 2
            b[p:p + 2] = data[o:o + 2]
    with wave.open(os.path.join(out, name + '.wav'), 'wb') as w:
        w.setnchannels(len(chans)); w.setsampwidth(2); w.setframerate(rate)
        w.writeframes(bytes(b))
print(f'{frames / rate:.1f} s -> {out}/mic.wav mics.wav ref.wav out.wav')
