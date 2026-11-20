#!/usr/bin/env python3
"""Cut a raw clip recording (16 kHz S32 stereo PDM) into words, normalise each
to a standard loudness and build the wake word test track.

usage: mkclips.py clips_raw.wav outdir [n_wake n_stop]
Writes outdir/clip_NN_<word>.wav, outdir/track.wav (each word once, 4 s apart,
1 s lead-in) and outdir/track.json (word + start time of every clip).
"""
import json
import os
import sys
import wave

import numpy as np

RATE = 16000
TARGET_DB = float(os.environ.get('TARGET_DB', -20.0))  # RMS of the speech part, dBFS (peaks capped at -1)
GAP = 4.0


def write(path, x):
    w = wave.open(path, 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(RATE)
    w.writeframes(np.clip(np.round(x * 32767), -32768, 32767).astype('<i2').tobytes()); w.close()


src, out = sys.argv[1], sys.argv[2]
n_wake, n_stop = (int(sys.argv[3]), int(sys.argv[4])) if len(sys.argv) > 4 else (10, 5)
os.makedirs(out, exist_ok=True)
w = wave.open(src)
a = np.frombuffer(w.readframes(w.getnframes()), dtype='<i4').reshape(-1, 2).astype(np.float64) / 2**31
x = a.mean(axis=1)
x[:3200] = 0
x -= np.convolve(x, np.ones(1601) / 1601, mode='same')  # remove DC / rumble
env = np.sqrt(np.convolve(x ** 2, np.ones(320) / 320, mode='same'))  # 20 ms
floor = np.percentile(env[3200:], 20)
on = env > floor * 6
# merge gaps < 250 ms, drop bursts < 200 ms
idx = np.flatnonzero(np.diff(on.astype(int)))
segs, i = [], 0
starts = list(np.flatnonzero(np.diff(np.r_[0, on.astype(int)]) == 1))
ends = list(np.flatnonzero(np.diff(np.r_[on.astype(int), 0]) == -1))
for s, e in zip(starts, ends):
    if segs and s - segs[-1][1] < 0.25 * RATE:
        segs[-1][1] = e
    else:
        segs.append([s, e])
segs = [s for s in segs if s[1] - s[0] > 0.12 * RATE]
print(f'noise floor {20 * np.log10(floor):.1f} dBFS, {len(segs)} words found')
if len(segs) != n_wake + n_stop:
    for s, e in segs:
        print(f'  {s / RATE:5.2f}-{e / RATE:5.2f} s')
    sys.exit(f'expected {n_wake + n_stop} words')
track = [np.zeros(RATE)]
meta, t = [], 1.0
for k, (s, e) in enumerate(segs):
    word = 'okay_nabu' if k < n_wake else 'stop'
    s, e = max(0, s - int(0.15 * RATE)), min(len(x), e + int(0.2 * RATE))  # keep soft edges
    c = x[s:e].copy()
    act = env[s:e] > floor * 6
    rms = np.sqrt(np.mean(c[act] ** 2))
    g = 10 ** (TARGET_DB / 20) / rms
    g = min(g, 10 ** (-1 / 20) / np.abs(c).max())
    c *= g
    fade = int(0.02 * RATE)
    c[:fade] *= np.linspace(0, 1, fade); c[-fade:] *= np.linspace(1, 0, fade)
    write(f'{out}/clip_{k:02d}_{word}.wav', c)
    meta.append({'word': word, 'start': round(t, 3), 'dur': round(len(c) / RATE, 3), 'gain_db': round(20 * np.log10(g), 1)})
    track += [c, np.zeros(int((GAP - len(c) / RATE) * RATE))]
    t += GAP
    print(f'  {word:9s} {len(c) / RATE:.2f} s  gain {20 * np.log10(g):+.1f} dB')
track.append(np.zeros(RATE))
write(f'{out}/track.wav', np.concatenate(track))
json.dump({'rate': RATE, 'target_rms_db': TARGET_DB, 'clips': meta}, open(f'{out}/track.json', 'w'), indent=1)
print(f'track.wav {sum(len(p) for p in track) / RATE:.1f} s')
