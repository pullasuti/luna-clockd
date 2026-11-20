#!/usr/bin/env python3
"""Find the DLNA volume at which the test words reach the clock as loud as a
person does (peaks through the current mic source, from /api/mic).

usage: calibrate.py --clips DIR --cookie FILE [--volumes 60,80,100]
"""
import argparse
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run  # noqa: E402

ap = argparse.ArgumentParser()
ap.add_argument('--clips', required=True)
ap.add_argument('--cookie', default='jarA.txt')
ap.add_argument('--volumes', default='60,80,100')
ap.add_argument('--seconds', type=float, default=14)
ap.add_argument('--clock', default=run.CLOCK)
ap.add_argument('--dot', default=run.DOT)
a = ap.parse_args()
run.set_targets(a.clock, a.dot)
clock = run.Clock(a.cookie)
root = os.path.dirname(os.path.abspath(a.clips))
port = run.serve(root)
url = f'http://{run.local_ip(run.urllib.parse.urlsplit(run.DOT).hostname)}:{port}/{os.path.basename(os.path.abspath(a.clips))}/track.wav'
print('source:', clock.req('/api/status').get('mic_source'))
for v in [int(x) for x in a.volumes.split(',')]:
    run.soap('RenderingControl', 'SetVolume', f'<Channel>Master</Channel><DesiredVolume>{v}</DesiredVolume>')
    run.soap('AVTransport', 'SetAVTransportURI', f'<CurrentURI>{url}</CurrentURI><CurrentURIMetaData></CurrentURIMetaData>')
    run.soap('AVTransport', 'Play', '<Speed>1</Speed>')
    peaks, t0 = [], time.time()
    while time.time() - t0 < a.seconds:
        m = clock.req('/api/mic')
        if m.get('fresh'):
            peaks.append(m['peak_db'])
        time.sleep(0.2)
    run.soap('AVTransport', 'Stop')
    peaks.sort()
    print(f'volume {v:3d}: loudest {peaks[-1]:.1f} dBFS, 90th percentile {peaks[int(len(peaks) * 0.9)]:.1f}, median {peaks[len(peaks) // 2]:.1f}')
    time.sleep(2)
