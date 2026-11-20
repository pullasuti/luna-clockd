#!/usr/bin/env python3
"""Two-mic echo null: learn the relative transfer function between the two
PDM mics for the speaker (H = S12/S22 on echo-only frames) and output
y = m1 - H * m2 (STFT, 512/256). The speaker is in the same housing a few cm
from both mics, a near-field path; a distant voice has a different relative
transfer, so it survives the null.

usage: nullmic.py learn.wav [apply.wav out.wav]
  learn.wav: 2-channel 16 kHz recording with the clock playing (echo dominates)
  apply.wav: 2-channel recording to process (default: learn.wav)
Prints how much the null removes from the echo and from a voice-only file
(VOICE env, 2-channel) if given.
"""
import os
import sys
import wave

import numpy as np

N, HOP = 512, 256
win = np.sqrt(np.hanning(N + 1)[:N])


def read2(p):
    w = wave.open(p)
    assert w.getnchannels() == 2 and w.getframerate() == 16000, p
    return np.frombuffer(w.readframes(w.getnframes()), dtype='<i2').reshape(-1, 2).astype(float) / 32768


def stft(x):
    n = (len(x) - N) // HOP
    return np.array([np.fft.rfft(win * x[i * HOP:i * HOP + N]) for i in range(n)])


def istft(X, length):
    y = np.zeros(length)
    for i, F in enumerate(X):
        y[i * HOP:i * HOP + N] += win * np.fft.irfft(F, N)
    return y


def learn(a):
    A, B = stft(a[:, 0]), stft(a[:, 1])
    e = (np.abs(A) ** 2).sum(axis=1)
    sel = e > np.percentile(e, 30)  # loud frames: the echo
    H = (A[sel] * np.conj(B[sel])).sum(0) / ((np.abs(B[sel]) ** 2).sum(0) + 1e-12)
    return H


def null(a, H):
    A, B = stft(a[:, 0]), stft(a[:, 1])
    return istft(A - H * B, len(a))


db = lambda x: 10 * np.log10(np.mean(x ** 2) + 1e-20)
lw = read2(sys.argv[1])
H = learn(lw[16000:])
y = null(lw, H)
print(f'echo: mic {db(lw[:, 0]):.1f} dB -> null {db(y):.1f} dB ({db(lw[:, 0]) - db(y):.1f} dB removed)')
if os.environ.get('VOICE'):
    v = read2(os.environ['VOICE'])
    yv = null(v, H)
    print(f'voice: mic {db(v[:, 0]):.1f} dB -> null {db(yv):.1f} dB ({db(v[:, 0]) - db(yv):.1f} dB removed)')
    # per band
    A, Y = stft(v[:, 0]), stft(yv)
    E, YE = stft(lw[:, 0]), stft(y)
    for lo, hi in ((100, 500), (500, 1000), (1000, 2000), (2000, 4000), (4000, 8000)):
        b = slice(lo * N // 16000, hi * N // 16000)
        f = lambda X: 10 * np.log10((np.abs(X[:, b]) ** 2).mean() + 1e-20)
        print(f'  {lo:4d}-{hi:4d} Hz: echo -{f(E) - f(YE):5.1f} dB, voice -{f(A) - f(Y):5.1f} dB')
if len(sys.argv) > 3:
    ap = read2(sys.argv[2])
    out = null(ap, H)
    w = wave.open(sys.argv[3], 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
    w.writeframes(np.clip(np.round(out * 32767), -32768, 32767).astype('<i2').tobytes()); w.close()
    print('wrote', sys.argv[3])
