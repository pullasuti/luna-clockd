#!/usr/bin/env python3
"""fascore.py - false-wake table from cmd/wwfile outputs over long room recordings.

    WWFLOOR=0.5 wwfile <models dir> rec/X.wav > score/X.txt     (one per chunk, see farec.py)
    fascore.py --hours 6 score/*.txt

Per model: bursts at >=0.5/0.8/0.9 and at the model's own cutoff, the highest score,
and the >=0.9 bursts with file and offset (to find them in the recording).
"""
import argparse, collections, os, re


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hours", type=float, required=True, help="recorded hours (for the per-hour column)")
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()
    ev = collections.defaultdict(list)
    for f in a.files:
        chunk = os.path.basename(f).split(".")[0]
        for line in open(f):
            m = re.match(r"\s+(\S+)\s+cutoff ([0-9.]+)\s+\[(.*)\]", line)
            if not m:
                continue
            key = (m.group(1), float(m.group(2)))
            ev[key]  # models with no bursts still get a row
            for x in m.group(3).split():
                at, sc = x.split(":")
                ev[key].append((chunk, float(at.rstrip("s")), float(sc)))
    print(f"{'model':20}{'cutoff':>7}{'>=0.5':>7}{'>=0.8':>7}{'>=0.9':>7}{'>=cut':>7}{'/hour':>7}  max")
    rows = sorted(ev.items(), key=lambda kv: (-sum(s >= kv[0][1] for *_, s in kv[1]), -len(kv[1])))
    for (n, c), e in rows:
        sc = [s for *_, s in e]
        hit = sum(s >= c for s in sc)
        print(f"{n:20}{c:7.2f}{len(sc):7}{sum(s >= .8 for s in sc):7}{sum(s >= .9 for s in sc):7}"
              f"{hit:7}{hit / a.hours:7.2f}  {max(sc, default=0):.2f}")
    print()
    for (n, c), e in rows:
        high = [f"{ch}+{at:.0f}s:{s:.2f}" for ch, at, s in e if s >= 0.9]
        if high:
            print(f"{n}: {' '.join(high)}")


if __name__ == "__main__":
    main()
