#!/usr/bin/env python3
"""facts_analyze.py - signal metrics of rtlfm facts-* captures (u8 I/Q, 1.92 MS/s).

    facts_analyze.py --offset -250000 [--rate 1920000] file.iq ...

Per capture: DC of I and Q (LSB), level (dBFS), I/Q power ratio and
correlation, the station's band power at --offset and at the mirror frequency
(-offset) over the noise floor, the strongest spectral peak, and the 19 kHz
stereo pilot after FM demodulation of the station (dB over 17.5 kHz).
A station at the expected side with a clean pilot = the chain works; a strong
mirror = spectrum inverted or I/Q broken.
"""
import argparse
import numpy as np
from scipy import signal


def band(f, p, lo, hi):
    m = (f >= lo) & (f < hi)
    return 10 * np.log10(np.mean(p[m]) + 1e-30)


def analyze(path, rate, off):
    raw = np.fromfile(path, dtype=np.uint8).astype(np.float64)
    if raw.size < 2 * 8192:
        return f"{path}: {raw.size} bytes, nothing to analyze"
    i, q = raw[0::2] - 127.5, raw[1::2] - 127.5
    x = i + 1j * q
    dci, dcq = i.mean(), q.mean()
    lvl = 10 * np.log10(np.mean(np.abs(x) ** 2) / 127.5 ** 2)
    iq_ratio = 10 * np.log10(np.var(i) / max(np.var(q), 1e-9))
    corr = np.corrcoef(i, q)[0, 1]
    f, p = signal.welch(x - x.mean(), fs=rate, nperseg=8192, return_onesided=False)
    f = np.fft.fftshift(f)
    p = np.fft.fftshift(p)
    noise = band(f, p, 0.60e6, 0.80e6)
    st = band(f, p, off - 80e3, off + 80e3) - noise
    mir = band(f, p, -off - 80e3, -off + 80e3) - noise
    pk = f[np.argmax(p * (np.abs(f) > 5e3))]
    # FM: move the station to 0, low-pass, 240 kS/s, discriminator, pilot
    n = np.arange(x.size)
    y = (x - x.mean()) * np.exp(-2j * np.pi * off / rate * n)
    y = signal.decimate(y, 8, ftype="fir")
    d = np.angle(y[1:] * np.conj(y[:-1]))
    fd, pd = signal.welch(d, fs=rate / 8, nperseg=4096)
    pilot = band(fd, pd, 18.9e3, 19.1e3) - band(fd, pd, 17.0e3, 18.0e3)
    return (f"{path.split('/')[-1]:10} DC {dci:+6.2f}/{dcq:+6.2f}  level {lvl:6.1f} dBFS  "
            f"I/Q {iq_ratio:+5.2f} dB corr {corr:+.3f}  noise {noise:6.1f}  "
            f"station {st:5.1f} dB  mirror {mir:5.1f} dB  peak {pk / 1e3:+7.1f} kHz  pilot {pilot:5.1f} dB")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rate", type=float, default=1.92e6)
    ap.add_argument("--offset", type=float, required=True, help="where the station should be, Hz")
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()
    for path in a.files:
        print(analyze(path, a.rate, a.offset))


if __name__ == "__main__":
    main()
