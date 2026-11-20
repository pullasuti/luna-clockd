#!/usr/bin/env python3
"""Replay a run.py capture through apmd variants on the workstation and score them.

usage: replay.py results.json [--music|--quiet] [--offset S] -- "name=apmd args" ...

Feeds the capture's canceller input (channel 0: after pre-gain and the two-mic
null) and speaker reference (channel 3) to a workstation build of apmd, block
by block as clockd does, and scores the output with the wake word models
(wwfile). "live" is what the clock produced during the run (channel 4).
A variant "name=@null ARGS" builds the input from the two raw mics with a null
learned offline on the whole capture (the best a null can do) instead.
Variants may add "duck=DB,MS": lower the echo part by DB from MS after each
clip starts (the quiet capture of the same run gives the voice; see --voice).

Env: APMD (x86 apmd), WWFILE, MODELS. Needs numpy.

For plain variant comparisons use cmd/micbench (clockd's own chain, ranges over
starting points, saved baselines); this script stays for the @null and duck
experiments.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import wave

import numpy as np

APMD, WW, MODELS = os.environ['APMD'], os.environ['WWFILE'], os.environ['MODELS']
T = os.path.join(os.environ.get('TMPDIR', '/tmp'), 'replay'); os.makedirs(T, exist_ok=True)
HOP = 160


def wr(p, x):
    with wave.open(p, 'wb') as o:
        o.setnchannels(1); o.setsampwidth(2); o.setframerate(16000)
        o.writeframes(np.clip(np.round(x), -32768, 32767).astype('<i2').tobytes())


def apmd(mic, ref, args):
    p = subprocess.Popen([APMD] + args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    hello = p.stdout.readline().split()
    assert hello[0] == b'ok' and int(hello[1]) == HOP, hello
    n = len(mic) // HOP
    m = np.clip(np.round(mic[:n * HOP]), -32768, 32767).astype('<i2').reshape(n, HOP)
    r = np.clip(np.round(ref[:n * HOP]), -32768, 32767).astype('<i2').reshape(n, HOP)
    out = np.zeros((n, HOP), '<i2')
    for i in range(n):
        p.stdin.write(m[i].tobytes() + r[i].tobytes()); p.stdin.flush()
        out[i] = np.frombuffer(p.stdout.read(2 * HOP), '<i2')
    p.stdin.close(); p.wait()
    return out.reshape(-1).astype(float)


def peaks(x):
    p = f'{T}/s.wav'; wr(p, x)
    out = subprocess.run([WW, MODELS, p], capture_output=True, text=True).stdout
    pk = {}
    for line in out.splitlines():
        m = re.match(r'\s+(\w+)\s+cutoff [0-9.]+\s+\[(.*)\]', line)
        if m:
            pk[m.group(1)] = [(float(a), float(b)) for a, b in re.findall(r'([0-9.]+)s:([0-9.]+)', m.group(2))]
    return pk


def score(x, clips, t0):
    pk = peaks(x)
    per = []
    for c in clips:
        a, b = t0 + c['start'] - 0.5, t0 + c['start'] + c['dur'] + 1.5
        s = [v for t, v in pk.get(c['word'], []) if a <= t <= b]
        per.append(max(s) if s else 0)
    return per


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('results')
    ap.add_argument('--quiet', action='store_true', help='the quiet capture (default: the music one)')
    ap.add_argument('--offset', type=float, help='clip time offset in the capture (default: the quiet run\'s)')
    ap.add_argument('variants', nargs='*')
    a = ap.parse_args()
    d = os.path.dirname(os.path.abspath(a.results))
    res = json.load(open(a.results))['results']
    r = [r for r in res if r['music'] != a.quiet and r.get('capture')][0]
    q = [r for r in res if not r['music']]
    off = a.offset if a.offset is not None else (q[0]['offset_s'] if q else r['offset_s'])
    t0 = r['capture_track_s'] + off
    clips = json.load(open(os.path.join(d, 'clips/track.json')))['clips']
    w = wave.open(os.path.join(d, r['capture']))
    x = np.frombuffer(w.readframes(w.getnframes()), '<i2').reshape(-1, 5).astype(float)
    mic, ref, live = x[:, 0], x[:, 3], x[:, 4]
    db = lambda v: 10 * np.log10(np.mean(v[16000:] ** 2) + 1e-9) - 90.3
    print(f'== {r["capture"]} ({len(x) / 16000:.0f} s), clips at +{t0:.2f} s; input {db(mic):.1f} dBFS, '
          f'reference {db(ref):.1f}, live output {db(live):.1f}')
    V = [('live', live)]
    for v in a.variants:
        name, _, args = v.partition('=')
        V.append((name, apmd(mic, ref, args.split())))
    for name, y in V:
        per = score(y, clips, t0)
        wake = [s for s, c in zip(per, clips) if c['word'] == 'okay_nabu']
        stop = [s for s, c in zip(per, clips) if c['word'] == 'stop']
        print(f'   {name:28s} wake >=.9 {sum(s >= .9 for s in wake)}/{len(wake)}  >=.5 {sum(s >= .5 for s in wake)}'
              f' | stop >=.5 {sum(s >= .5 for s in stop)}/{len(stop)} | out {db(y):6.1f} dBFS | '
              + ' '.join(f'{s:.2f}' for s in per))


if __name__ == '__main__':
    main()
