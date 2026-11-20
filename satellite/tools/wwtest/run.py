#!/usr/bin/env python3
"""Repeatable wake word test: plays the recorded words from a DLNA speaker
(the Echo Dot running Kiosk Satellite) at a fixed volume and reads what the
clock heard, for each echo canceller, with and without music on the clock.

usage: run.py --clips DIR [--sources vendor,local] [--music FILE] [--volume 40]
              [--cookie FILE] [--out results.json] [--record DIR]

Needs a logged-in clockd session cookie (curl cookie jar). The clock runs in
wake test mode during each run: hits are logged but start no turns, and "stop"
doesn't stop anything. DIR is mkclips.py's output (track.wav + track.json).

--record DIR saves what the clock's microphone path saw during each run with
the WebRTC / LocalVQE sources (not Original) as DIR/<when>-<source>-<quiet|music>.wav:
16 kHz, 5 channels: mic (canceller input), mic 1, mic 2,
speaker reference, output. The result records where in the file the track
started (capture_track_s), so clip times map onto it: track start + clip start.
"""
import argparse
import functools
import http.server
import json
import os
import re
import socket
import socketserver
import statistics
import struct
import threading
import time
import urllib.parse
import urllib.request
from datetime import datetime

# set with --clock / --dot (or CLOCK / DOT in the environment)
CLOCK = os.environ.get('CLOCK', 'http://lenovo-clock.local')
DOT = os.environ.get('DOT', '')


def set_targets(clock, dot):
    global CLOCK, DOT
    if not dot:
        raise SystemExit('the DLNA speaker is required: --dot http://<speaker-ip>:<port> (or DOT=...)')
    CLOCK, DOT = clock.rstrip('/'), dot.rstrip('/')


def local_ip(dest):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.connect((dest, 9))
    return s.getsockname()[0]


class Clock:
    def __init__(self, cookie_jar):
        tok = None
        for line in open(cookie_jar):
            f = line.rstrip('\n').split('\t')
            if len(f) == 7 and f[5] == 'clockd_session':
                tok = f[6]
        if not tok:
            raise SystemExit(f'no clockd_session in {cookie_jar}')
        self.h = {'Cookie': f'clockd_session={tok}', 'Origin': CLOCK}

    def req(self, path, data=None, method=None):
        body = json.dumps(data).encode() if data is not None else None
        h = dict(self.h, **({'Content-Type': 'application/json'} if body else {}))
        r = urllib.request.Request(CLOCK + path, data=body, headers=h, method=method or ('POST' if body is not None else 'GET'))
        return json.loads(urllib.request.urlopen(r, timeout=10).read() or b'{}')

    def action(self, name, value=0):
        return self.req(f'/api/action/{name}?value={value}', data=None, method='POST')

    def set_source(self, src):
        cfg = self.req('/api/config')['config']
        cfg['voice']['mic_source'] = src
        self.req('/api/config', cfg)
        for _ in range(30):
            if self.req('/api/status').get('mic_source') == src:
                return
            time.sleep(1)
        raise SystemExit(f'clock did not switch to {src}: {self.req("/api/status").get("mic_source_error")}')


class Capture:
    """Streams GET /api/capture into a WAV file on a thread."""

    def __init__(self, clock, path, seconds):
        self.path, self.t0, self.err, self.bytes = path, None, None, 0
        r = urllib.request.Request(CLOCK + f'/api/capture?seconds={seconds:.0f}', headers=clock.h)
        self.resp = urllib.request.urlopen(r, timeout=10)
        self.th = threading.Thread(target=self._run, daemon=True)
        self.th.start()

    def _run(self):
        try:
            with open(self.path, 'wb') as f:
                while True:
                    b = self.resp.read(32768)
                    if not b:
                        break
                    if self.t0 is None:
                        # the first chunk holds the header and ~0.1 s of audio
                        self.t0 = time.time() - max(0, len(b) - 44) / (16000 * 5 * 2)
                    f.write(b)
                    self.bytes += len(b)
        except Exception as e:
            self.err = e

    def finish(self):
        self.th.join(30)
        n = os.path.getsize(self.path) - 44
        n -= n % 10  # whole frames
        with open(self.path, 'r+b') as f:  # the stream had no length: fix the header
            f.truncate(44 + n)
            f.seek(4); f.write(struct.pack('<I', 36 + n))
            f.seek(40); f.write(struct.pack('<I', n))
        return n / (16000 * 5 * 2)


def soap(svc, action, args=''):
    body = (f'<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" '
            f's:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:{action} '
            f'xmlns:u="urn:schemas-upnp-org:service:{svc}:1"><InstanceID>0</InstanceID>{args}</u:{action}></s:Body></s:Envelope>')
    r = urllib.request.Request(f'{DOT}/control/{svc}', data=body.encode(), method='POST', headers={
        'Content-Type': 'text/xml; charset="utf-8"', 'SOAPACTION': f'"urn:schemas-upnp-org:service:{svc}:1#{action}"'})
    return urllib.request.urlopen(r, timeout=5).read().decode()


def dot_state():
    return re.search(r'<CurrentTransportState>(\w+)<', soap('AVTransport', 'GetTransportInfo')).group(1)


def serve(directory):
    class H(http.server.SimpleHTTPRequestHandler):
        def log_message(self, *a):
            pass
    srv = socketserver.ThreadingTCPServer(('0.0.0.0', 0), functools.partial(H, directory=directory))
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv.server_address[1]


def parse_at(s):
    s = re.sub(r'(\.\d{6})\d+', r'\1', s)  # Go writes nanoseconds
    return datetime.fromisoformat(s).timestamp()


def match(events, clips, t0):
    """Find the time offset that lines events up with the clips (clock and
    workstation clocks and the speaker's start-up delay all differ), then
    assign each event to its clip."""
    want = lambda e: 'stop' if e.get('stop') else 'okay_nabu'
    ends = [t0 + c['start'] + c['dur'] for c in clips]
    best = (-1, 0)
    for k in range(-200, 201):  # -10 .. +10 s
        off = k * 0.05
        n = sum(1 for e in events for i, c in enumerate(clips)
                if want(e) == c['word'] and abs(e['t'] - off - ends[i]) < 0.8)
        if n > best[0]:
            best = (n, off)
    off = best[1]
    per = [{'word': c['word'], 'hit': None, 'near': None} for c in clips]
    extra = []
    for e in events:
        i = min(range(len(clips)), key=lambda i: abs(e['t'] - off - ends[i]))
        if want(e) != clips[i]['word'] or abs(e['t'] - off - ends[i]) > 1.5:
            extra.append(e)
            continue
        key = 'hit' if e['kind'] == 'detected' else 'near'
        if per[i][key] is None or e['score'] > per[i][key]:
            per[i][key] = e['score']
    return per, extra, off


def run_one(clock, src, music_url, track_url, clips, dur, volume, record=None):
    clock.set_source(src)
    prev_vol = None
    if music_url:
        # music.wav is a tap of the speaker signal (after the user's volume): play it at 100 %
        prev_vol = clock.req('/api/status').get('volume')
        clock.action('volume', 1)
        clock.req('/api/play', {'url': music_url})
    time.sleep(12)  # canceller / AGC settle, music under way
    lv = []
    for _ in range(6):
        m = clock.req('/api/mic')
        if m.get('fresh'):
            lv.append(m['rms_db'])
        time.sleep(0.5)
    cap = None
    if record and src != 'vendor':
        name = f"{time.strftime('%Y%m%d-%H%M')}-{src}-{'music' if music_url else 'quiet'}.wav"
        cap = Capture(clock, os.path.join(record, name), dur + 12)
    clock.action('wake_test', dur + 20)
    soap('RenderingControl', 'SetVolume', f'<Channel>Master</Channel><DesiredVolume>{volume}</DesiredVolume>')
    soap('AVTransport', 'SetAVTransportURI', f'<CurrentURI>{track_url}</CurrentURI><CurrentURIMetaData></CurrentURIMetaData>')
    soap('AVTransport', 'Play', '<Speed>1</Speed>')
    t0 = time.time()
    time.sleep(dur + 3)
    soap('AVTransport', 'Stop')  # this renderer repeats the track until told to stop
    capinfo = {}
    if cap:
        secs = cap.finish()
        capinfo = {'capture': cap.path, 'capture_s': round(secs, 2),
                   'capture_track_s': round(t0 - cap.t0, 3) if cap.t0 else None,
                   'capture_error': str(cap.err) if cap.err else None}
        print(f'  saved {cap.path} ({secs:.0f} s)' + (f', error {cap.err}' if cap.err else ''))
    hist = clock.req('/api/wake/history')['events']
    clock.action('wake_test', 0)
    if music_url:
        clock.req('/api/play', {'url': ''})
        if prev_vol is not None:
            clock.action('volume', prev_vol)
    evs = [dict(e, t=parse_at(e['at'])) for e in hist if e.get('test')]
    evs = [e for e in evs if e['t'] > t0 - 15]
    per, extra, off = match(evs, clips, t0)
    return {'source': src, 'music': bool(music_url), 'floor_db': statistics.median(lv) if lv else None,
            'per_clip': per, 'unmatched': [{k: e[k] for k in ('kind', 'word', 'score')} for e in extra],
            'offset_s': round(off, 2), **capinfo}


def report(r):
    w = [p for p in r['per_clip'] if p['word'] == 'okay_nabu']
    s = [p for p in r['per_clip'] if p['word'] == 'stop']
    def line(name, ps):
        hits = sum(1 for p in ps if p['hit'] is not None)
        near = sum(1 for p in ps if p['hit'] is None and p['near'] is not None)
        best = [max(x for x in (p['hit'], p['near']) if x is not None) for p in ps if p['hit'] is not None or p['near'] is not None]
        return f'{name} {hits}/{len(ps)} heard, {near} near misses, {len(ps) - hits - near} nothing' + \
            (f', scores {min(best):.2f}-{max(best):.2f}' if best else '')
    print(f"{r['source']:6s} {'music' if r['music'] else 'quiet':5s} floor {r['floor_db']} dBFS | "
          f"{line('wake', w)} | {line('stop', s)} | extra {len(r['unmatched'])}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--clips', required=True)
    ap.add_argument('--sources', default='vendor,local')
    ap.add_argument('--music')
    ap.add_argument('--volume', type=int, default=90)  # calibrated 2026-11-03: clips at -20 dBFS RMS
    ap.add_argument('--cookie', default='jarA.txt')
    ap.add_argument('--out', default='results.json')
    ap.add_argument('--record', metavar='DIR', help='save the microphone path of each run (WebRTC/LocalVQE)')
    ap.add_argument('--clock', default=CLOCK, help='clockd web UI, e.g. http://lenovo-clock.local')
    ap.add_argument('--dot', default=DOT, help='DLNA renderer control base, e.g. http://192.0.2.10:2325')
    a = ap.parse_args()
    set_targets(a.clock, a.dot)
    meta = json.load(open(os.path.join(a.clips, 'track.json')))
    clips = meta['clips']
    dur = clips[-1]['start'] + clips[-1]['dur'] + 1
    root = os.path.dirname(os.path.abspath(a.clips))
    port = serve(root)
    ip = local_ip(urllib.parse.urlsplit(DOT).hostname)
    track_url = f'http://{ip}:{port}/{os.path.basename(os.path.abspath(a.clips))}/track.wav'
    music_url = None
    if a.music:
        mus = os.path.abspath(a.music)
        if not mus.startswith(root + '/'):
            raise SystemExit(f'--music must be inside {root}')
        music_url = f'http://{ip}:{port}/{os.path.relpath(mus, root)}'
    clock = Clock(a.cookie)
    if a.record:
        os.makedirs(a.record, exist_ok=True)
    orig = clock.req('/api/config')['config']['voice'].get('mic_source') or 'vendor'
    orig_vol = clock.req('/api/status').get('volume')  # restored however the run ends
    results = []
    try:
        for music in ([None, music_url] if music_url else [None]):
            for src in a.sources.split(','):
                r = run_one(clock, src, music, track_url, clips, dur, a.volume, a.record)
                report(r)
                results.append(r)
    finally:
        try:
            soap('AVTransport', 'Stop')
        except Exception:
            pass
        for f in (lambda: clock.action('wake_test', 0), lambda: clock.req('/api/play', {'url': ''}),
                  lambda: clock.action('volume', orig_vol), lambda: clock.set_source(orig)):
            try:
                f()
            except Exception as e:
                print('cleanup:', e)
    json.dump({'when': time.strftime('%Y-%m-%d %H:%M'), 'volume': a.volume, 'results': results},
              open(a.out, 'w'), indent=1)
    print('saved', a.out)


if __name__ == '__main__':
    main()
