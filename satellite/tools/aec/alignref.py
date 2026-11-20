#!/usr/bin/env python3
"""Align clockd's speaker tap (48 kHz stereo s16) to a 16 kHz mic recording and
write it as a 16 kHz mono reference that leads its echo by LEAD_MS (default 40).

usage: alignref.py mic.wav ref48.wav out_ref16.wav
"""
import os
import sys
import wave

import numpy as np


def read(p):
    w = wave.open(p)
    a = np.frombuffer(w.readframes(w.getnframes()), dtype='<i2').reshape(-1, w.getnchannels()).astype(float) / 32768
    return a, w.getframerate()


mic, mr = read(sys.argv[1]); ref, rr = read(sys.argv[2])
assert mr == 16000 and rr == 48000
m = mic.mean(axis=1); m -= m.mean()
n = np.arange(-96, 97); h = np.sinc(2 * 7000 / 48000 * n) * np.hamming(len(n)); h /= h.sum()
r = np.convolve(ref.mean(axis=1), h, mode='same')[::3]
L = min(len(m), len(r), 16000 * 30)
N = 1 << int(np.ceil(np.log2(2 * L)))
X = np.fft.rfft(m[:L], N) * np.conj(np.fft.rfft(r[:L], N))
cc = np.fft.irfft(X / (np.abs(X) + 1e-12), N)
# mic[k] ~ ref[k - lag]; the tap may start before or after the mic recording
lags = np.r_[np.arange(0, 16000 * 3), np.arange(N - 16000 * 3, N)]
k = lags[np.argmax(cc[lags])]
lag = k if k < N // 2 else k - N
srt = np.sort(cc[lags])[::-1]
lead = int(float(os.environ.get('LEAD_MS', 40)) * 16)
print(f'echo at {lag / 16:.1f} ms after the tap (peak {cc[k]:.3f}, next {srt[100]:.3f}); reference placed {lead / 16:.0f} ms ahead')
out = np.zeros(len(m))
sh = lag - lead
src0, dst0 = (0, sh) if sh >= 0 else (-sh, 0)
cnt = min(len(r) - src0, len(m) - dst0)
out[dst0:dst0 + cnt] = r[src0:src0 + cnt]
w = wave.open(sys.argv[3], 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
w.writeframes(np.clip(np.round(out * 32767), -32768, 32767).astype('<i2').tobytes()); w.close()
