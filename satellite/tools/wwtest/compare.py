#!/usr/bin/env python3
"""Offline comparison of echo cancellers on recorded wake test runs with music.

usage: compare.py RESULTS.json [...]   (run.py --record results; uses their music run)

For each music capture: builds variants from the recorded mics + reference,
scores them with the wake word models (wwfile) and reports per clip the best
okay_nabu / stop score near each clip. Gains: the output is tried at 0/+12/+24 dB
and the best is shown (the models are level sensitive; the live path has AGC).
Env: APMTOOL, WWFILE, MODELS, VQE (vqe.py), VQEPY (python with numpy for it),
VQEMODELS (dir with the .gguf files).
"""
import json, os, re, subprocess, sys, wave
import numpy as np

APM = os.environ['APMTOOL']; WW = os.environ['WWFILE']; MODELS = os.environ['MODELS']
VQE, VQEPY, VQEM = os.environ['VQE'], os.environ['VQEPY'], os.environ['VQEMODELS']
T = os.path.join(os.environ.get('TMPDIR', '/tmp'), 'compare'); os.makedirs(T, exist_ok=True)
# stft, istft, learn, null from nullmic.py (defined by exec: linters can't see them)
exec(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), '../aec/nullmic.py')).read()
     .split('db = lambda')[0].split('import numpy as np', 1)[1])  # stft, istft, learn, null


def wr(p, x):
    with wave.open(p, 'wb') as o:
        o.setnchannels(1); o.setsampwidth(2); o.setframerate(16000)
        o.writeframes(np.clip(np.round(x), -32768, 32767).astype('<i2').tobytes())


def rd(p):
    return np.frombuffer(wave.open(p).readframes(10 ** 9), '<i2').astype(float)


def score(x, clips, t0):
    best = None
    for g in (0, 12, 24):
        p = f'{T}/s.wav'; wr(p, x * 10 ** (g / 20))
        out = subprocess.run([WW, MODELS, p], capture_output=True, text=True).stdout
        peaks = {}
        for line in out.splitlines():
            m = re.match(r'\s+(\w+)\s+cutoff [0-9.]+\s+\[(.*)\]', line)
            if m:
                peaks[m.group(1)] = [(float(a), float(b)) for a, b in re.findall(r'([0-9.]+)s:([0-9.]+)', m.group(2))]
        per = []
        for c in clips:
            a, b = t0 + c['start'] - 0.3, t0 + c['start'] + c['dur'] + 1.5
            s = [v for t, v in peaks.get(c['word'], []) if a <= t <= b]
            per.append(max(s) if s else 0)
        wake = [s for s, c in zip(per, clips) if c['word'] == 'okay_nabu']
        stop = [s for s, c in zip(per, clips) if c['word'] == 'stop']
        r = (sum(s >= .8 for s in wake), sum(s >= .5 for s in wake), sum(s >= .5 for s in stop), g, len(wake), len(stop))
        if best is None or r[:3] > best[:3]:
            best = r
    return best


clips = json.load(open('clips/track.json'))['clips']
for res in sys.argv[1:]:
    r = [r for r in json.load(open(res))['results'] if r['music'] and r.get('capture')][0]
    t0 = r['capture_track_s'] + r['offset_s']
    w = wave.open(r['capture']); x = np.frombuffer(w.readframes(w.getnframes()), '<i2').reshape(-1, 5).astype(float)
    mic, L, R, ref, live = x[:, 0], x[:, 1], x[:, 2], x[:, 3], x[:, 4]
    wr(f'{T}/mic.wav', mic); wr(f'{T}/ref.wav', ref)
    print(f'== {res}: {r["capture"]} ({len(x) / 16000:.0f} s)')
    V = {}
    V['live WebRTC output'] = live
    V['mic, no processing'] = mic

    def apm(name, args, micp=f'{T}/mic.wav'):
        subprocess.run([APM, '--pre', '0'] + args + [micp, f'{T}/o.wav', f'{T}/ref.wav'], check=True, capture_output=True)
        V[name] = rd(f'{T}/o.wav')

    def vqe(name, model, micp=f'{T}/mic.wav', pre=-34):
        # the LocalVQE models expect raw-ish levels (live: +6 dB on the PDM signal = -34 dB from here)
        m = rd(micp) * 10 ** (pre / 20); wr(f'{T}/vm.wav', m)
        subprocess.run([VQEPY, VQE, os.path.join(VQEM, model), f'{T}/vm.wav', f'{T}/ref.wav', f'{T}/o.wav'], check=True, capture_output=True)
        V[name] = rd(f'{T}/o.wav')

    apm('AEC3 only', ['--ns', '-1', '--agc1', 'off'])
    apm('AEC3 + NS high', ['--ns', '2', '--agc1', 'off'])
    vqe('LocalVQE 2.7K (live model)', 'localvqe-v1.4-aec-2.7K-f32.gguf')
    vqe('LocalVQE pi-aec 49K', 'localvqe-pi-aec-v1-49k-f32.gguf')
    H = learn(np.stack([L, R], 1) / 32768)
    nul = null(np.stack([L, R], 1) / 32768, H) * 32768
    e = lambda v: 10 * np.log10(np.mean(v[16000:] ** 2) + 1e-9)
    print(f'   two-mic null: echo {e(L):.1f} -> {e(nul):.1f} dB')
    V['two-mic null'] = nul
    wr(f'{T}/nul.wav', nul)
    apm('null -> AEC3', ['--ns', '-1', '--agc1', 'off'], micp=f'{T}/nul.wav')
    vqe('null -> LocalVQE pi-aec 49K', 'localvqe-pi-aec-v1-49k-f32.gguf', micp=f'{T}/nul.wav')
    vqe('null -> LocalVQE 2.7K (live model)', 'localvqe-v1.4-aec-2.7K-f32.gguf', micp=f'{T}/nul.wav')
    wr(f'{T}/a3.wav', V['AEC3 only'])
    vqe('AEC3 -> LocalVQE pi-aec 49K', 'localvqe-pi-aec-v1-49k-f32.gguf', micp=f'{T}/a3.wav')
    for name, y in V.items():
        w8, w5, s5, g, nw, ns = score(y, clips, t0)
        print(f'   {name:32s} wake >=.8 {w8}/{nw}  >=.5 {w5}/{nw} | stop >=.5 {s5}/{ns}   (gain +{g} dB)')
