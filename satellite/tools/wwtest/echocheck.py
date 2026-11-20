#!/usr/bin/env python3
"""echocheck.py capture.wav PRE_DB: echo check on a test capture (run.py --record
or /api/capture) taken while the clock played sound. Prints the echo lag behind
the reference, levels, and apmtool replays (AEC3 alone, with NS/AGC, shifted
reference). PRE_DB puts the mic channel at the canceller's level: 40 for v0.49.0
captures, 0 from v0.49.1. Needs numpy; APMTOOL = path to apmtool (tools/aec)."""
import sys, wave, subprocess, os
import numpy as np
cap, pre = sys.argv[1], float(sys.argv[2])  # pre: gain to put the mic channel at the canceller's level
T = os.path.dirname(os.path.abspath(cap)) + '/echo'
os.makedirs(T, exist_ok=True)
w = wave.open(cap); x = np.frombuffer(w.readframes(w.getnframes()), '<i2').reshape(-1, 5).astype(float)
mic, ref, out = x[:, 0] * 10 ** (pre / 20), x[:, 3], x[:, 4]
db = lambda v: 20 * np.log10(np.sqrt(np.mean(v ** 2)) / 32768 + 1e-12)
act = np.convolve(np.abs(ref) > 300, np.ones(1600), 'same') > 0  # tune playing (100 ms smoothing)
q = ~act
q[:3200] = False
print(f'tune {act.mean() * len(act) / 16000:.1f} s of {len(act) / 16000:.1f} s')
print(f'levels during tune / quiet:  ref {db(ref[act]):.1f}   mic {db(mic[act]):.1f} / {db(mic[q]):.1f}   live out {db(out[act]):.1f} / {db(out[q]):.1f}')
# echo lag: cross-correlation of mic with ref (GCC-PHAT)
n = 1 << int(np.ceil(np.log2(2 * len(mic))))
M, R = np.fft.rfft(mic, n), np.fft.rfft(ref, n)
g = np.fft.irfft(M * np.conj(R) / (np.abs(M * np.conj(R)) + 1e-9), n)
lags = np.concatenate([g[-8000:], g[:8000]]); lag = np.argmax(lags) - 8000
print(f'echo lag (mic behind ref): {lag / 16:.1f} ms  (peak {lags.max() / np.median(np.abs(lags)):.0f}x median)')
def wav(p, v, ch=1):
    with wave.open(p, 'wb') as o:
        o.setnchannels(ch); o.setsampwidth(2); o.setframerate(16000)
        o.writeframes(np.clip(np.round(v), -32768, 32767).astype('<i2').tobytes())
wav(T + '/mic.wav', mic); wav(T + '/ref.wav', ref)
A = os.environ.get('APMTOOL', 'apmtool')
def run(name, args, refp=T + '/ref.wav', micp=T + '/mic.wav'):
    o = T + '/o.wav'
    r = subprocess.run([A, '--pre', '0'] + args + [micp, o] + ([refp] if refp else []), capture_output=True, text=True)
    if r.returncode:
        print(name, 'failed', r.stderr[-300:]); return
    y = np.frombuffer(wave.open(o).readframes(10 ** 9), '<i2').astype(float)
    print(f'  {name:42s} out tune {db(y[act[:len(y)]]):6.1f}  quiet {db(y[q[:len(y)]]):6.1f}  -> tune is {db(y[act[:len(y)]]) - db(y[q[:len(y)]]):+5.1f} dB over quiet')
print('replays (apmtool, same input):')
run('AEC3 only (no NS/AGC)', ['--ns', '-1', '--agc1', 'off'])
run('AEC3 + NS 1 + AGC (like apmd?)', ['--ns', '1', '--agc1', 'digital'])
run('AEC3 + NS 1, no AGC', ['--ns', '1', '--agc1', 'off'])
# reference shifted: does alignment matter?
for sh in (-40, 40, 80):
    s = int(sh * 16)
    r2 = np.roll(ref, s); wav(T + '/ref_s.wav', r2)
    run(f'AEC3 only, reference {sh:+d} ms later', ['--ns', '-1', '--agc1', 'off'], refp=T + '/ref_s.wav')
