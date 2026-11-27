#!/usr/bin/env python3
"""provcheck.py - code-provenance similarity check (radio code vs GPL sources).

    provcheck.py [--k 10] [--show 3] --ref <dir with reference .c/.h> <our files...>

Two measures per (our file, reference file) pair:

  1. Structural shingles: both files are tokenized, comments and string
     literals dropped, identifiers and keywords replaced by "I", numbers by
     "N"; then every run of k tokens is hashed. "shared" counts our runs that
     also occur in the reference. This finds copied or translated code shape
     even after renaming, while short idioms (k small) stay noise. Go and C
     differ in syntax, so only longer shared runs mean something.
  2. Constant sequences: the numeric literals of each function-sized chunk, in
     order. A run of >= 4 identical constants in the same order (e.g. a
     register init sequence) is reported with its line numbers on both sides.

Reference sources are read from --ref and never copied anywhere; the script
prints only line numbers and constants. Reviewers decide what a hit means:
register addresses and values are hardware facts; the same sequence with the
same comments is derivation.
"""
import argparse
import hashlib
import os
import re
import sys

TOK = re.compile(r"""
    (?P<comment>//[^\n]*|/\*.*?\*/)
  | (?P<str>"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|`[^`]*`)
  | (?P<num>0[xX][0-9a-fA-F_]+|\d[\d_]*(?:\.\d+)?(?:[eE][-+]?\d+)?)
  | (?P<id>[A-Za-z_]\w*)
  | (?P<op><<=|>>=|<<|>>|&&|\|\||[-+*/%&|^!=<>]=?|[{}()\[\];,.?:~])
""", re.S | re.X)


def tokens(src):
    out = []  # (kind, text, line)
    line = 1
    pos = 0
    for m in TOK.finditer(src):
        line += src.count("\n", pos, m.start())
        pos = m.start()
        k = m.lastgroup
        if k in ("comment", "str"):
            continue
        t = m.group(0)
        out.append((k, t, line))
    return out


def norm_num(t):
    t = t.replace("_", "").lower()
    try:
        return int(t, 0)
    except ValueError:
        try:
            v = float(t)
            return int(v) if v == int(v) else v
        except ValueError:
            return t


def shingles(toks, k):
    norm = []
    for kind, t, ln in toks:
        if kind == "id":
            norm.append(("I", ln))
        elif kind == "num":
            norm.append(("N", ln))
        else:
            norm.append((t, ln))
    h = {}
    for i in range(len(norm) - k + 1):
        key = hashlib.blake2b(" ".join(x for x, _ in norm[i:i + k]).encode(), digest_size=8).digest()
        h.setdefault(key, norm[i][1])
    return h


def const_seq(toks):
    # numbers that look like register data: skip 0, 1, 2 (loop/index noise)
    return [(norm_num(t), ln) for kind, t, ln in toks if kind == "num" and norm_num(t) not in (0, 1, 2)]


def common_runs(a, b, minlen):
    """Longest common contiguous runs of constants (a, b lists of (value, line))."""
    runs = []
    av = [x for x, _ in a]
    bv = [x for x, _ in b]
    seen = set()
    for i in range(len(av)):
        for j in range(len(bv)):
            if av[i] != bv[j] or (i, j) in seen:
                continue
            n = 0
            while i + n < len(av) and j + n < len(bv) and av[i + n] == bv[j + n]:
                seen.add((i + n, j + n))
                n += 1
            if n >= minlen:
                runs.append((n, a[i][1], a[i + n - 1][1], b[j][1], b[j + n - 1][1], av[i:i + n]))
    return sorted(runs, reverse=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ref", required=True)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--min-consts", type=int, default=4)
    ap.add_argument("--show", type=int, default=3)
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()
    refs = {}
    for f in sorted(os.listdir(a.ref)):
        if f.endswith((".c", ".h")):
            t = tokens(open(os.path.join(a.ref, f), errors="replace").read())
            refs[f] = (t, shingles(t, a.k), const_seq(t))
    for f in a.files:
        t = tokens(open(f).read())
        sh = shingles(t, a.k)
        cs = const_seq(t)
        print(f"== {f}  ({len(t)} tokens, {len(sh)} {a.k}-token runs)")
        for r, (rt, rsh, rcs) in refs.items():
            shared = [ln for key, ln in sh.items() if key in rsh]
            runs = common_runs(cs, rcs, a.min_consts)
            if not shared and not runs:
                continue
            pct = 100 * len(shared) / max(1, len(sh))
            lines = sorted(set(shared))
            print(f"  {r:16} shape: {len(shared):4} shared runs ({pct:4.1f} %)"
                  + (f", our lines {lines[0]}-{lines[-1]}" if lines else ""))
            for n, a1, a2, b1, b2, vals in runs[:a.show]:
                vs = " ".join(hex(v) if isinstance(v, int) else str(v) for v in vals[:12])
                print(f"      {n:2} constants in order: ours L{a1}-{a2} / ref L{b1}-{b2}: {vs}{' ...' if n > 12 else ''}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
