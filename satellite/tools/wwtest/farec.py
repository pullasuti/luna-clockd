#!/usr/bin/env python3
"""farec.py - record hours of room audio from a clock for false-wake benchmarks.

Logs in to the clock's web UI (approve with the action button, or pass --approve <adb
serial> to press it over adb), then loops GET /api/capture (max 30 min per request) and
keeps channel 4 ("out": exactly what the wake word models get) as 16 kHz mono WAV chunks:

    farec.py --clock http://<ip> --out <dir> [--hours 8] [--chunk 1800] [--approve <serial>]

Score the chunks offline with cmd/wwfile (tools/wwtest/fascore.py). Logs out at the end.
"""
import argparse, http.cookiejar, json, os, struct, subprocess, sys, time, urllib.request

CH, OUT_CH, RATE = 5, 4, 16000


def opener(clock):
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    op.addheaders = [("Origin", clock)]
    return op


def login(op, clock, approve):
    r = json.load(op.open(urllib.request.Request(clock + "/api/auth/request", data=b"", method="POST")))
    print(f"login code {r['code']}: press the clock's action button", flush=True)
    if approve:
        subprocess.run(["adb", "-s", approve, "shell",
                        "sendevent /dev/input/event0 1 139 1; sendevent /dev/input/event0 0 0 0; sleep 1; "
                        "sendevent /dev/input/event0 1 139 0; sendevent /dev/input/event0 0 0 0"], check=False)
    for _ in range(60):
        s = json.load(op.open(f"{clock}/api/auth/poll?id={r['id']}"))["state"]
        if s == "approved":
            return
        if s != "pending":
            sys.exit(f"login {s}")
        time.sleep(1)
    sys.exit("login timed out")


def wav_header(n):
    return (b"RIFF" + struct.pack("<I", 36 + n) + b"WAVEfmt " +
            struct.pack("<IHHIIHH", 16, 1, 1, RATE, RATE * 2, 2, 16) + b"data" + struct.pack("<I", n))


def record_chunk(op, clock, seconds, path):
    resp = op.open(f"{clock}/api/capture?seconds={seconds}", timeout=60)
    head = b""
    while b"data" not in head or len(head) < head.find(b"data") + 8:
        b = resp.read(64)
        if not b:
            raise IOError("capture ended in the header")
        head += b
    rest = head[head.find(b"data") + 8:]
    frame = 2 * CH
    n = 0
    with open(path + ".part", "wb") as f:
        f.write(wav_header(0))
        while True:
            b = resp.read(frame * 1024)
            if not b:
                break
            rest += b
            k = len(rest) // frame * frame
            blk, rest = rest[:k], rest[k:]
            mono = b"".join(blk[i + 2 * OUT_CH:i + 2 * OUT_CH + 2] for i in range(0, k, frame))
            f.write(mono)
            n += len(mono)
        f.seek(0)
        f.write(wav_header(n))
    os.replace(path + ".part", path)
    return n / 2 / RATE


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--clock", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--hours", type=float, default=8)
    ap.add_argument("--chunk", type=int, default=1800)
    ap.add_argument("--approve", help="adb serial: press the action button to approve the login")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    op = opener(a.clock)
    login(op, a.clock, a.approve)
    end = time.time() + a.hours * 3600
    total = 0.0
    try:
        while time.time() < end:
            name = time.strftime("%Y%m%d-%H%M%S") + ".wav"
            try:
                sec = record_chunk(op, a.clock, min(a.chunk, int(end - time.time()) + 1), os.path.join(a.out, name))
                total += sec
                print(f"{name}: {sec:.0f} s (total {total / 3600:.2f} h)", flush=True)
            except Exception as e:  # clock restart, Wi-Fi blip: wait and go on
                print(f"{name}: {e}", flush=True)
                time.sleep(30)
    finally:
        try:
            op.open(urllib.request.Request(a.clock + "/api/auth/logout", data=b"", method="POST"))
        except Exception:
            pass


if __name__ == "__main__":
    main()
