#!/usr/bin/env python3
"""Run LocalVQE (C API via ctypes) on a 16 kHz mono mic/ref WAV pair.

usage: vqe.py model.gguf mic.wav ref.wav out.wav
Prints the time taken and the level of mic, ref and output.
"""
import ctypes
import os
import sys
import time
import wave

import numpy as np

LIB = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'LocalVQE/ggml/build/bin/liblocalvqe.so')


def read16(path):
    w = wave.open(path)
    assert w.getframerate() == 16000 and w.getnchannels() == 1 and w.getsampwidth() == 2, path
    return np.frombuffer(w.readframes(w.getnframes()), dtype='<i2').astype(np.float32) / 32768


def write16(path, x):
    w = wave.open(path, 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
    w.writeframes(np.clip(np.round(x * 32767), -32768, 32767).astype('<i2').tobytes()); w.close()


db = lambda x: 20 * np.log10(np.sqrt(np.mean(np.square(x, dtype=np.float64))) + 1e-12)

model, micp, refp, outp = sys.argv[1:5]
lib = ctypes.CDLL(LIB)
lib.localvqe_new.restype = ctypes.c_size_t
lib.localvqe_new.argtypes = [ctypes.c_char_p]
lib.localvqe_last_error.restype = ctypes.c_char_p
lib.localvqe_last_error.argtypes = [ctypes.c_size_t]
lib.localvqe_process_f32.argtypes = [ctypes.c_size_t, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_int, ctypes.c_void_p]
lib.localvqe_free.argtypes = [ctypes.c_size_t]
for f in ('localvqe_sample_rate', 'localvqe_hop_length'):
    getattr(lib, f).argtypes = [ctypes.c_size_t]

ctx = lib.localvqe_new(model.encode())
if not ctx:
    sys.exit('load failed')
mic, ref = read16(micp), read16(refp)
n = min(len(mic), len(ref))
hop = lib.localvqe_hop_length(ctx)
n -= n % hop
mic, ref = np.ascontiguousarray(mic[:n]), np.ascontiguousarray(ref[:n])
out = np.zeros(n, dtype=np.float32)
t0 = time.perf_counter()
rc = lib.localvqe_process_f32(ctx, mic.ctypes.data, ref.ctypes.data, n, out.ctypes.data)
dt = time.perf_counter() - t0
if rc != 0:
    sys.exit('process failed: %s' % lib.localvqe_last_error(ctx))
lib.localvqe_free(ctx)
write16(outp, out)
print(f'{os.path.basename(model)}: {n / 16000:.1f} s audio in {dt * 1000:.0f} ms ({n / 16000 / dt:.0f}x real time, workstation); '
      f'mic {db(mic):.1f} dBFS, ref {db(ref):.1f}, out {db(out):.1f} dBFS')
