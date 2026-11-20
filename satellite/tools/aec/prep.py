#!/usr/bin/env python3
"""Turn a raw mic recording (16 kHz S32 stereo) and clockd's speaker tap
(48 kHz s16 stereo) into the 16 kHz mono pair LocalVQE wants, aligned by
cross-correlation, and report the echo path delay and level.

usage: prep.py mic.wav ref.wav out_prefix [gain_db]
"""
import os
import sys
import wave

import numpy as np


def read(path):
    w = wave.open(path)
    n, ch, sw, rate = w.getnframes(), w.getnchannels(), w.getsampwidth(), w.getframerate()
    raw = w.readframes(n)
    a = np.frombuffer(raw, dtype={2: '<i2', 4: '<i4'}[sw]).reshape(-1, ch).astype(np.float64)
    return a / (2 ** (8 * sw - 1)), rate


def write(path, x, rate=16000):
    y = np.clip(np.round(x * 32767), -32768, 32767).astype('<i2')
    w = wave.open(path, 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(rate); w.writeframes(y.tobytes()); w.close()


def resample_48_to_16(x):
    # windowed-sinc low-pass at 7 kHz, then take every third sample
    n = np.arange(-96, 97)
    h = np.sinc(2 * 7000 / 48000 * n) * np.hamming(len(n)); h /= h.sum()
    return np.convolve(x, h, mode='same')[::3]


mic, mr = read(sys.argv[1]); ref, rr = read(sys.argv[2]); out = sys.argv[3]
gain = 10 ** (float(sys.argv[4]) / 20) if len(sys.argv) > 4 else 1.0
assert mr == 16000 and rr == 48000, (mr, rr)
m = mic[:, 0] - mic[:, 0].mean()
m[:3200] = 0  # start-up click
r = resample_48_to_16(ref.mean(axis=1))
# where does the reference start inside the mic recording? (energy envelope xcorr)
env = lambda x: np.sqrt(np.convolve(x ** 2, np.ones(160) / 160, mode='same'))
em, er = env(m), env(r)
xc = np.correlate(em - em.mean(), er - er.mean(), mode='full')
lag = xc.argmax() - (len(er) - 1)  # mic index = ref index + lag
# continuous music fools the envelope match: LAG_MS (from GCC-PHAT, see gcc.py) overrides it
if os.environ.get('LAG_MS'):
    lag = int(round(float(os.environ['LAG_MS']) * 16))
print(f'reference starts {lag / 16000:+.3f} s into the mic recording (incl. speaker/air delay)')
# put both on the mic's timeline, the reference 40 ms ahead of its echo (as it is
# in real life: a canceller needs the reference first)
lag -= 640
r_al = np.zeros_like(m)
if lag >= 0:
    k = min(len(r), len(m) - lag); r_al[lag:lag + k] = r[:k]
else:
    k = min(len(r) + lag, len(m)); r_al[:k] = r[-lag:-lag + k]
# fine delay with sample xcorr around the coarse lag (the canceller estimates it too)
seg = slice(lag if lag > 0 else 0, min(len(m), (lag if lag > 0 else 0) + 32000))
fx = np.correlate(m[seg], r_al[seg], mode='full'); mid = len(r_al[seg]) - 1
win = fx[mid - 800: mid + 800]; fine = win.argmax() - 800
print(f'fine echo delay vs that start: {fine / 16:.1f} ms')
act = em > 3 * np.median(em)
print(f'mic level while the speaker plays: {20 * np.log10(np.sqrt((m[act] ** 2).mean()) + 1e-12):.1f} dBFS; quiet: {20 * np.log10(np.median(em) + 1e-12):.1f} dBFS')
write(out + '_mic.wav', m * gain); write(out + '_ref.wav', r_al)
print('wrote', out + '_mic.wav', out + '_ref.wav', f'(mic gain {20 * np.log10(gain):+.0f} dB)')
