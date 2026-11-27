# Engineering notes

Findings behind clockd's design: measurements, dead ends and why things are done the way
they are. These are dated excerpts from the project's working notes, newest work last;
later sections can correct earlier ones (they say so). Code comments that cite
"Engineering Notes <date>" point to the section with that date here.

Three clocks were used: **clock #1** (retail firmware, daily use and the wake word test
rig), **clock #2** (an engineering build, the test bed) and **clock #3** (also an engineering
build, a second test bed).

## Contents

- [Release packages and the update engine (2026-11-09, clockd v0.78.0)](#release-packages-and-the-update-engine-2026-11-09-clockd-v0780)
- [Setup hotspot without net_mgr (2026-11-09)](#setup-hotspot-without-net_mgr-2026-11-09)
- [DHCP without the vendor dhcpcd (2026-11-09)](#dhcp-without-the-vendor-dhcpcd-2026-11-09)
- [Wi-Fi control socket, and why a network change went back (2026-11-10)](#wi-fi-control-socket-and-why-a-network-change-went-back-2026-11-10)
- [2026-11-10: the two-mic null deepened to a 4096 frame; music-run numbers](#2026-11-10-the-two-mic-null-deepened-to-a-4096-frame-music-run-numbers)
- [2026-11-10 (later): listening closer (wake-score ducking), null learning hold](#2026-11-10-later-listening-closer-wake-score-ducking-null-learning-hold)
- [2026-11-10 (evening): micbench: the mic path as an offline measurement](#2026-11-10-evening-micbench-the-mic-path-as-an-offline-measurement)
- [2026-11-10 (night): reference timing in our own capture: steady, nothing to fix](#2026-11-10-night-reference-timing-in-our-own-capture-steady-nothing-to-fix)
- [2026-11-10 (night): LocalVQE 49k after the two-mic null: no gain, real-time work dropped](#2026-11-10-night-localvqe-49k-after-the-two-mic-null-no-gain-real-time-work-dropped)
- [2026-11-10 (night): USB-A port: wired, but device mode only (corrects 2026-11-07)](#2026-11-10-night-usb-a-port-wired-but-device-mode-only-corrects-2026-11-07)
- [2026-11-10 (night): USB host mode at run time: one register write, a stick mounts](#2026-11-10-night-usb-host-mode-at-run-time-one-register-write-a-stick-mounts)
- [Setup hotspot: button confirm and optional password (2026-11-11, clockd v0.85.0)](#setup-hotspot-button-confirm-and-optional-password-2026-11-11-clockd-v0850)
- [Back to stock, vendor reset, and the hotspot channel move (2026-11-11, v0.86.0-0.86.1)](#back-to-stock-vendor-reset-and-the-hotspot-channel-move-2026-11-11-v0860-0861)
- [USB: the panel (v0.91.0), the xHCI death, the NLS limit, and the lost stick (2026-11-11)](#usb-the-panel-v0910-the-xhci-death-the-nls-limit-and-the-lost-stick-2026-11-11)
- [USB rework (v0.91.1): what emptied the stick, and the switching glitches (2026-11-12)](#usb-rework-v0911-what-emptied-the-stick-and-the-switching-glitches-2026-11-12)
- [2026-11-12: FLAC decoder (v0.92.0)](#2026-11-12-flac-decoder-v0920)
- [2026-11-12: FLAC to HA and Sendspin (v0.93.0)](#2026-11-12-flac-to-ha-and-sendspin-v0930)
- [2026-11-12b: how HA picks the audio format for an ESPHome satellite](#2026-11-12b-how-ha-picks-the-audio-format-for-an-esphome-satellite)
- [2026-11-12c: voice answers stayed WAV because of the SPEAKER feature flag](#2026-11-12c-voice-answers-stayed-wav-because-of-the-speaker-feature-flag)
- [2026-11-12d: the single speaker plays left + right summed](#2026-11-12d-the-single-speaker-plays-left--right-summed)
- [2026-11-12e: the amp can be switched off when idle without a pop](#2026-11-12e-the-amp-can-be-switched-off-when-idle-without-a-pop)
- [2026-11-12f: clockd v0.94.0 switches the amp off when idle](#2026-11-12f-clockd-v0940-switches-the-amp-off-when-idle)
- [2026-11-12g: USB player transport line: mewkiz seek pitfalls (v0.96.0)](#2026-11-12g-usb-player-transport-line-mewkiz-seek-pitfalls-v0960)
- [Bluetooth: the radio works; "dead radio" was a bug in our probe (2026-11-12, clock #2)](#bluetooth-the-radio-works-dead-radio-was-a-bug-in-our-probe-2026-11-12-clock-2)
- [clockd Bluetooth proxy, and four kernel-interface traps (2026-11-12, clock #2, v0.98.0)](#clockd-bluetooth-proxy-and-four-kernel-interface-traps-2026-11-12-clock-2-v0980)
- [Power measurement gotcha: the USB cable is ~1.3 W (2026-11-12, clock #2)](#power-measurement-gotcha-the-usb-cable-is-13-w-2026-11-12-clock-2)
- [2026-11-14: wake word false-wake benchmark (6 h evening TV/talk, clock #3)](#2026-11-14-wake-word-false-wake-benchmark-6-h-evening-tvtalk-clock-3)
- [2026-11-14: rain on a tin roof - settled on v1, six dead ends](#2026-11-14-rain-on-a-tin-roof---settled-on-v1-six-dead-ends)
- [2026-11-14: unattended run - MP3, update path, setup DNS, SSH keys, DLNA (v0.101-0.106)](#2026-11-14-unattended-run---mp3-update-path-setup-dns-ssh-keys-dlna-v0101-0106)
- [Relocked clock running clockd (2026-11-17)](#relocked-clock-running-clockd-2026-11-17)

## Release packages and the update engine (2026-11-09, clockd v0.78.0)

Why this shape: an update is root code execution, and the web UI upload (next step) runs over
plain http from any logged-in browser, so the clock must check who made the package, not only
that it arrived intact.

- Package: `clockd-<ver>-arm64.tar.gz` = manifest.json (format 1, product clockd, version,
  model CD-4N341Y, files with size/SHA-256/mode), manifest.sig (Ed25519 over the manifest
  bytes, base64), then `files/<path>`. Only clockd, start.sh, restart.sh, setup-ap.sh, bin/*,
  aec/*, models/* may be in it (internal/release Allowed); never settings, keys, /system.
  The whole stage.sh tree is ~12 MB, 8.8 MB packed.
- Key: Ed25519, private seed in `~/.config/clockd-release/release.key` on the build machine
  (0600, made with `go run ./tools/release keygen`; back it up: a lost key means the next
  update needs adb). Public keys in internal/release/keys.go; rotation = add the new key,
  ship one release signed with the old key, drop the old key later. `release pack` refuses a
  key clockd doesn't trust.
- Install (`release.Install`, also behind `clockd -install`): flock, refuse while a trial or
  swap marker exists, verify the signature before reading any file, check free space, stage
  only files that differ (models are add-only) in update/new, check ELFs are arm64 and that the
  new clockd runs `-version` and reports the manifest's version, then swap by rename (old file
  -> update/prev, new -> place; renaming works on running binaries), list added files in
  update/added, `swapping` marker during the swap, renamed to `trial` at the end.
- Confirmation: clockd removes update/trial after 60 s of running (update/last records it).
  start.sh counts starts in update/tries while trial exists; on the 4th start (three exits
  inside the trial) or a leftover `swapping` it moves update/prev back, deletes the added
  files and writes update/failed (clockd logs it). Measured on clock #2: a build exiting at
  start was back on the old version ~10 s after the restart.
- Limits: a hang without exit isn't caught; start.sh changes only take effect after a reboot
  (the running supervisor is the old script); the first install still needs adb and the full
  install route.
- Web UI route (v0.79.0): the upload is stored in update/upload.tar.gz (removed after install,
  cancel or a clockd restart) and checked with release.Check before the page offers Install;
  Install re-reads the same file through release.Install (full check again). On clock #2: a
  9 MB package checked in ~1.6 s, installed in ~4 s, back up as the new version ~5 s later.

## Setup hotspot without net_mgr (2026-11-09)

- The vendor /bin/hostapd (both builds' copy on clock #2 checked) switches itself to the wifi
  user before reading its config: strace shows prctl(PR_SET_KEEPCAPS, 1), setgid32(1008),
  setuid32(1008), then openat(config) = EACCES and exit_group(1). /data/local is root 0700, so
  any config under /data/local fails silently (logcat: only "IEEE 802.11: Steering
  disabled"). This was the 2026-11-04 "hostapd by hand exits at once" - not the chip, not
  net_mgr.
- With net_mgr stopped, config in /tmp/aptest (0644), ctrl_interface dir owned wifi:wifi
  0770, `ifconfig uap0 up`, `/bin/hostapd -e /tmp/aptest/entropy.bin /tmp/aptest/hostapd.conf`:
  uap0 type AP, "AP/GO Link up", wlan0 stays COMPLETED. strace is on the eng build
  (/system/xbin/strace).
- What net_mgr does for `hotspot start` (logcat HotspotManager/WifiUtil): uap0 up; reads
  wlan0's channel (STA on 5 GHz ch 36 -> "starting AP on channel 6 2.4G"); writes
  /tmp/hostapd.conf (interface=uap0, nl80211, ctrl_interface=/data/wifi, channel, g, 11n,
  ssid); runs `/bin/hostapd -e /data/wifi/hostapd_entropy.bin /tmp/hostapd.conf` as wifi
  with CapEff 0x3000 (NET_ADMIN + NET_RAW); 192.168.255.249/29 on uap0; DHCP-only dnsmasq.
  While wlan0 is connected on ch 36 the driver moves the AP to ch 36 anyway
  (wl_ext_move_cur_channel).
- v0.80.0 on clock #2: setup-ap.sh runs hostapd itself (busybox chown needs the numeric
  1008:1008, it knows no Android user names). Starting net_mgr (the fallback route) logs
  "Net_Mgr starting", "turn off power-save", and HttpTimeSync resolves clients1.google.com and
  syncs the time over HTTP port 80 within 0.2 s - so net_mgr contacts Google on every start,
  including at boot before start.sh stops it.
- Versions: retail (clock #1) wpa_supplicant/hostapd v2.7-1.52, dnsmasq 2.51 (2009);
  eng (clock #2) v2.7-1.56, dnsmasq 2.78; both dhcpcd 6.8.2, busybox 1.21.1 (2013), kernel
  4.9.113. Listening: adbd 5555 (all interfaces), clockd 80/6053/8928, mDNS 5353, dhcpcd 68.
- /etc/resolv.conf (vendor dhcpcd hook): 8.8.8.8 and 2001:4860:4860::8888 listed before the
  router's DNS, 8.8.4.4 after.

## DHCP without the vendor dhcpcd (2026-11-09)

- /etc/resolv.conf is a symlink to /tmp/resolv.conf (RAM): rewriting it costs no flash.
- The vendor wpa_cli also drops to the wifi user (uid 1008), like hostapd: its -a action
  script can't read /data/local (root 0700) or signal root processes - and in -a mode it
  delivered no events at all (a debug action in /tmp never ran across several
  CTRL-EVENT-CONNECTED). Polling `wpa_cli status` from a root loop works.
- /sys/class/net/wlan0/carrier and operstate stay 1/up through a reassociate: no use as a
  link signal.
- busybox udhcpc always runs "deconfig" at start; dhcp.sh skips the first one when it takes
  over an address (marker file) and asks for that address again (-r), so the takeover keeps
  <clock-ip> without a gap. init's `stop dhcpcd` kills it without a DHCP release.
- The vendor dhcpcd left a metric-305 default route; ours replaces all default routes with
  one via the lease's router.

## Wi-Fi control socket, and why a network change went back (2026-11-10)

- wpa_supplicant's control socket is `/dev/socket/wpa_wlan0` on both builds: init.rc gives
  the service `socket wpa_wlan0 dgram 660 wifi wifi`, and this Android build takes that
  socket instead of creating one under `ctrl_interface=/data/wifi` (that directory holds
  only the config). Root can use it; a
  client bound to an abstract-namespace name gets its replies without any file.
- Events do arrive after ATTACH on a plain datagram client (the vendor wpa_cli's `-a` mode
  delivered none): `<3>CTRL-EVENT-CONNECTED - Connection to <bssid> completed [id=N id_str=]`,
  `CTRL-EVENT-DISCONNECTED bssid= reason= locally_generated=`, `CTRL-EVENT-SSID-TEMP-DISABLED
  id= ssid= auth_failures= duration= reason=WRONG_KEY` (about 8 s after a join with a wrong
  password), `CTRL-EVENT-NETWORK-NOT-FOUND` once per scan (three scans = about 21 s).
- The "joined, then went back" change, replayed with raw commands on clock #2 (two saved
  networks on one subnet, the old one stronger): `SELECT_NETWORK 1` connects in 4 s;
  `ENABLE_NETWORK all` alone changes nothing; the next `SCAN` result disconnects and
  associates with network 0 within 0.3 s. With `SET_NETWORK 1 priority 1` the same scan
  leaves it on network 1. So select + enable-all needs a priority on the chosen network;
  DHCP had no part in it.
- udhcpc after a failed join: the clock was off Wi-Fi for 11+ s, udhcpc removed the address
  (deconfig) and got it back about 8 s after the reconnect (one leasefail in between).

## 2026-11-10: the two-mic null deepened to a 4096 frame; music-run numbers

Offline on a fresh rig capture (run_20261110_base, tools/wwtest/replay.py, new,
replays a capture's canceller input + reference through a workstation apmd and
scores with wwfile; a `default` replay matched the live clock's quiet and music
scores exactly):

- The null's depth is set by its frame length: H = mic1/mic2 is a long, two-sided
  room response and one factor per bin can only stand for a response much shorter
  than the frame. 512/256 (the old null.go): 21 dB of echo out; 1024: 25; 2048:
  28; 4096: 29.5 dB: where the two mics' own coherence limit ends (30 dB,
  measured). The voice lost under 1 dB at every length. A causal FIR built from
  the same H did worse than the 512 frame at any length tried (up to 4096 taps):
  the relative path is not causal-invertible, so the STFT delay is the price.
- The reference-based AEC3 on top of the null has almost nothing left to take:
  its linear filter removed 3-7 dB more, the mic2->mic1 coherence bound is 30 dB
  and the ref->mic bound is time-variant (per-10 s −20 dB, whole-file −8.5),
  which explains why pinning a fixed delay (--delay-ms, added to apmd as an
  experiment along with --aec-linear/--aec-mask/--filter-blocks) always scored
  worse than letting it search.
- An energy-based voice detector for ducking (per-band null output vs raw mic)
  does not separate: the voice is ~3 dB of rise inside a ±3 dB spread; no
  threshold crosses. Ducking would have to trigger on the wake score itself.
- While the clock plays music, the processed output never scores above 0.00 on
  music-only audio (132 s, both runs): every non-zero score is a real word.

null.go now runs 4096/2048 (a fixed 256 ms delay on the mic and the reference
while the null is on; old null.bin files are the wrong size and relearn from
scratch). Live on clock #1 (v0.83.2, rig): reduction 27.3 dB reported, quiet
wake 5-8/10 across three runs (was 4/10 that morning), music wake 0/10 at the
0.9 threshold but 4/10 ≥ 0.5 and one 0.74; music stop word 4/5 (was 0/5).
The ≥3/10 at the standard threshold still needs the 49k filter (CPU-bound) or
ducking.

## 2026-11-10 (later): listening closer (wake-score ducking), null learning hold

- microWakeWord's score rises late in the word, so a duck triggered by it mostly
  helps the *next* attempt; that is the design (v0.84.0): score >= 0.15 while
  music plays -> media group gain 0.12 for 4 s, extended while scores keep coming.
  Trigger level from the rig captures with wwfile's new WWFLOOR=0.05: music
  alone (5 music runs, ~350 s incl. the dips) never above 0.05; missed words at
  full volume peaked 0.1-0.77.
- Rig (clock #1, music 38 %, live thresholds 0.9 quiet / 0.8 playing): run 1
  (dip to 0.2) 2/10 heard, run 2 (dip to 0.12 + learning hold) 3/10; inside
  the dip the words scored 0.51-0.88. Quiet runs the same afternoon: 6/10 and
  4/10: the Dot's clips are marginal in quiet too, so the dip cannot do better
  than that on this rig.
- First dip run: after the dip the next four words scored 0.00 in every replay
  variant (AEC on, off, linear). Cause: the null kept learning during the dip,
  when the Dot's voice was no longer far below the echo, and learned to remove
  the voice. Learning is now held while the music is down for a voice (dip,
  listening). General rule: the null may only learn when the echo dominates.
- AEC3 on top of the null is a lottery at full volume: live vs a bit-exact-input
  replay differ per clip (0.00 vs 0.5+), and default / --aec-linear / --aec-mask
  4 / 30 / --no-aec all land at 1-4 of 10 above 0.5 with different clips each.
  When AEC3 has converged it suppresses the residual *and* the voice (output
  -60..-69 dBFS); when it has not, the null alone lets words through at 0.5-0.7.
- Null cost on the A53 (go test -bench on clock #2): 0.60 ms per 16 ms playing
  (one 4096 FFT for both mics + one inverse), 0.07 ms idle (time-domain
  window-and-add, identical output).
- Capture ownership: init.rc starts iot_audio at boot; start.sh now stops it
  and clockd checks `init.svc.iot_audio` before its stop + 1 s wait.

## 2026-11-10 (evening): micbench: the mic path as an offline measurement

`satellite/cmd/micbench` replays rig captures (5 channels: canceller input, two
mics, reference, live output) through `localmic.Replay`, the chain the live
loop itself uses, and scores the output with the wake word models.

    cd <folder with the rig captures>
    export APMD=<x86 apmd> MODELS=<repo>/satellite/models
    <repo>/satellite/build/micbench -save bench.json run_20261110_*.json
    <repo>/satellite/build/micbench -base bench.json -starts 8 -only music run_*.json "default=" "x=--aec-mask 4"

Findings from the first runs (captures of 2026-11-10):

- Deterministic: the same capture and options give the same output hash on
  every run. The earlier "replay does not match live" was not randomness.
- PC (x86 apmd) vs the clock (arm64 apmd, micbench run on clock #2, 267 s for
  284 s of audio next to a running clockd): hashes differ, per-clip scores are
  within ~0.05 except one clip (0.07 vs 0.30). The platform is a small factor.
- The starting state is the large one. Capture "dip2", music: fresh helper
  0.50 0.63 0.66 0.65 0.93 ..., helper primed with the same capture
  0.43 0.92 0.65 0.45 0.88 ..., live 0.20 0.91 0.67 0.43 0.93 ... (primed is
  close to live: the live helper had been running). Dropping 63 ms steps from
  the front moves the count at 0.8 between 2 and 4 of 10 on one capture, and
  even with the AEC off (1-3 at 0.9): part of it is where the wake model's own
  frames fall on the word.
- So a single run or replay can't rank two variants. With `-starts 8` on three
  music captures: default / --no-aec / --aec-linear are inside each other's
  ranges (mean peak 0.62/0.64/0.59, 0.20/0.24/0.26, 0.25/0.33/0.29); --no-aec
  loses the stop word (mean 0.2-1.0 of 5 vs 0.8-2.5) and raised the highest
  music-only score to 0.37 once. Confirms keeping the defaults.
- Limits: the bench can't know the live chain's state at the start of a
  capture; the music dip and the null's learning hold happened in the room
  (stage full with -warm learns through the dips and so removes some voice);
  quiet captures replay within 0.1 of live per clip.

## 2026-11-10 (night): reference timing in our own capture: steady, nothing to fix

Question: is the echo canceller's reference badly timed (the "api jitter up to
~140 ms" note above), and does that explain why identical rig runs differ?

- That jitter note was about the vendor's canceller fed over the mixer-service
  loopback. Our pipeline pairs reference and microphone sample by sample from a
  queue (`localmic` `onMix`/`takeRef`), so there is no such jitter to remove.
- Measured on the saved captures (cross-correlation of the reference against a
  raw microphone, 0.5 s windows, 12 music captures of 71 s): within a run the
  echo's delay is constant to the sample (141 of 141 windows the same value)
  in all but two captures, which each have one step (+19 samples, +115
  samples = 7 ms). The +115 step is one hop of zeros where the queue ran dry;
  the canceller's suppression 2 s later is the same as before it.
- Between runs the delay differs: 52, 74, 86, 92, 94 ms seen. It is where the
  queue happens to settle at start. AEC3 finds its delay itself, and a fixed
  delay scored worse before, so this is left alone.
- So the run-to-run spread (0-3 of 10 on one capture) is the canceller's
  convergence and scores near the threshold, not timing. Analysis scripts were
  throwaway (scratch `an15`-`an17`); the method above is enough to repeat it.
- The null's output (capture channel 0 with the null on) is useless for this
  measurement: its filter moves the correlation peak. Use a raw mic channel
  and undo the reference's 4096-sample null delay.

## 2026-11-10 (night): LocalVQE 49k after the two-mic null: no gain, real-time work dropped

Question: is the 49k GTCRN model worth making real-time on the A53 (it is ~30 %
too slow)? Checked offline first with micbench on the three music captures
(meas, learn, dip2), 8 starting points each. LocalVQE (@7f61cd0, DAF N=32)
built for the PC; a throwaway helper chained vqed -> apmd `--no-aec` (NS3 +
AGC2), so only the echo canceller differs from the default.

Wake words of 10 (range over the 8 starts, mean in brackets), stop of 5:

    capture          canceller   >=0.8        >=0.5        stop >=0.5   elsewhere
    meas (1155)      AEC3        0-1 (0.2)    1-2 (1.5)    2-3 (2.5)    0.07
                     49k         0-1 (0.1)    1-3 (2.1)    0-2 (1.2)    0.03
                     2.7K        0-0 (0.0)    1-3 (1.9)    0-2 (1.0)    0.03
    learn (1152)     AEC3        1-1 (1.0)    1-4 (2.2)    0-1 (0.8)    0.23
                     49k         1-2 (1.1)    1-4 (2.8)    0-1 (0.5)    0.68
                     2.7K        1-1 (1.0)    1-4 (2.9)    0-1 (0.6)    0.31
    dip2 (1632)      AEC3        2-4 (2.9)    6-7 (6.6)    1-1 (1.0)    0.07
                     49k         2-4 (2.9)    5-8 (6.8)    0-2 (1.1)    0.05
                     2.7K        2-4 (3.1)    6-8 (7.2)    0-2 (1.1)    0.04

- The three cancellers sit inside each other's ranges for the wake word. 49k
  is no better than the 2.7K model, loses the stop word on one capture and
  gave the one near false alarm (0.68 away from any clip).
- Why: the null already takes ~30 dB of the echo out (raw mic 68.9 dB ->
  38.6 dB on meas). What is left is what the two mics don't share with the
  speaker path, and a reference-based canceller finds little in it: 49k
  removes 3 dB more, 2.7K 1 dB. On the raw mic alone 49k removes 27 dB, the
  same as the null, and no canceller then scores anything (`-stage raw`: all
  0.00-0.15): the null is what makes the wake word audible, not the canceller
  behind it.
- The 2026-11-03 result (null + 49k 3/10 vs null + AEC3 1/10) was with the
  short 512-point null; the 4096-point one (v0.83.2) closed that gap.
- Decision: no real-time 49k work. Local (LocalVQE 2.7K) stays a selectable
  source but has no measured advantage over WebRTC. First-try wakes over music
  now rest on the dip, not on a better canceller.
- micbench got `-stage raw` (one raw microphone, no null). The dip is baked
  into the dip2 capture as it happened.

## 2026-11-10 (night): USB-A port: wired, but device mode only (corrects 2026-11-07)

The 2026-11-07 section says the USB-A port's data lines are not wired. Wrong: adb
over USB and burn mode use exactly that port (user). What the running kernels show
(clock #2 eng and clock #1 retail, read-only, identical on both):

- One USB2 PHY (`ffe09000.usb2phy`) shared by two controllers: `ff500000.dwc3`
  (xHCI host, buses usb1/usb2, registered but with nothing on them) and
  `ff400000.dwc2_a` (dwc_otg, the adb gadget; `/sys/class/udc`).
- Device tree: `dwc2_a` has `controller-type = 3` (OTG) but `port-type = 2`;
  dmesg: "dwc_otg: Working on port type = SLAVE". `usb3phy` has `otg = 1`, so
  the vendor PHY driver would switch on the ID signal.
- The ID signal says "device" on both clocks, with or without a cable:
  PHY glue `usb_r5` (0xffe09094) = 0x0000FF73, `usb_r0` (0xffe09080) =
  0x8000006A (bit 31, u2d_act: PHY routed to the device controller), GOTGCTL =
  0x001d0000 (ConIDSts = B-device). A USB-A socket has no ID pin, so the board
  ties it to "device".
- So a stick is never enumerated: the PHY is never handed to the xHCI host.
  That matches the 2026-11-07 observation (zero kernel USB events on replug or
  at boot). Kernel side is ready for storage (CONFIG_USB_STORAGE, BLK_DEV_SD,
  vfat via the vendor's mount line).

Not tried: forcing host mode at run time (what the PHY driver's ID-change work
does: clear u2d_act, wake the host port; via /dev/mem), or a device tree with
the port as host (boot image work). Unknown: whether the socket supplies 5 V
to a stick on its own (it is sold as a charging port, so probably yes). In
host mode USB adb on that clock is gone (Wi-Fi adb stays); a reboot restores
device mode.

mountd (`/sbin/mount_usb_drive.sh`) is stopped by start.sh since tonight (flag
file `use-mountd` keeps it): with the port in device mode it can mount nothing.

## 2026-11-10 (night): USB host mode at run time: one register write, a stick mounts

Follow-up to the section above. Works on clock #2 (eng); not yet tried on retail.

    /data/local/clockd/bin/busybox devmem 0xffe09020 32 0x00250000

- What it does: 0xffe09020 is `u2p_r0` of PHY port 1 (the only live port; port
  0 reads all zeros). It was 0x00252000; the write clears bit 13, `idpullup`.
  The socket has no ID pin, so without the pull-up the ID signal reads low =
  "host". The vendor PHY driver (`phy-aml-new-usb3.c`, `otg = 1` in the device
  tree) gets its ID interrupt and does the switch itself: `usb_r0` 0x8000006A ->
  0x0000006A (u2d_act off), `usb_r4` 2 -> 0, D+/D- pull-downs on (`u2p_r0`
  reads 0x00250060), `usb_r5` 0xFF73 -> 0xFF30, and it tells dwc_otg the device
  side is off ("USB_STATE=DISCONNECTED"). Nothing was written by hand except
  the pull-up bit.
- Dead ends first: `usb_r5`'s iddig_reg / iddig_cfg bits do not override the
  ID signal (iddig_reg reads back 1 whatever is written).
- Result: the clock left the PC's USB bus (adb over USB gone, Wi-Fi adb fine).
  The user then plugged a 32 GB stick into the socket: enumerated on the xHCI
  bus at high speed (480 Mbit/s), usb-storage -> sda1, and the vendor mountd
  (started by hand for the test) mounted it vfat at /data/usb within seconds.
  The socket powers the stick by itself (stick asks for 300 mA).
- Back to device mode: a reboot. Tested on clock #2: with the PC cable back in
  the clock stayed in host mode (not on the PC's bus) until `adb reboot`; after it
  all four registers read their boot values again (0x00252000, 0x8000006A, 2,
  0xFF73), the gadget is "configured", the clock is back on the PC's USB bus and
  mountd is stopped (start.sh). Nothing persists. Not tried: setting the pull-up bit again
  (0x00252000 should make the driver switch back the same way), hot-unplug,
  hubs, other device classes (the kernel has usb-audio, HID, UVC, cdc_ether,
  asix built in), retail firmware.
- Caution: mountd mounts read-write with fmask/dmask 0000. Nothing on the clock
  runs anything from /data/usb; keep it that way (a stick must never be a way
  to root: signed packages only).
- Open design questions: who decides the mode (a setting, or host by default
  with device mode on request for adb/burn), and that USB adb and a stick
  exclude each other on the one socket.

## Setup hotspot: button confirm and optional password (2026-11-11, clockd v0.85.0)

Review item 6. The setup hotspot was open, so anyone in radio range could join it and
use the setup page: point the clock at their own network, or read the home Wi-Fi password
as it is typed (the page is plain HTTP over an open link).

- **Button confirm** (every clock): the page asks, the face shows a 4-digit code, the
  page shows the same one, and holding the action button for 0.7 s confirms it (the same
  hold as the web login). Before that, scan and connect answer 403. The confirm comes
  BEFORE the form, so the home Wi-Fi password is never sent to an unconfirmed page. Limits:
  one waiting page, 60 s to press, 15 min once confirmed, id bound to the address that
  asked, everything forgotten when the hotspot stops. This answers *who* may send; it
  does not hide *what* is sent.
- **Hotspot password** (optional, `install-clock.sh --hotspot-password`): WPA2-PSK/CCMP.
  The key is derived on the PC (PBKDF2-SHA1 against the hotspot name, as for the Wi-Fi
  key) and stored as `/data/local/clockd/hotspot.key` (64 hex, root 0600); the password
  never reaches the clock, so it can't be shown or recovered. hostapd.conf now holds a
  key, so it is written 0600 owned by the wifi user (1008), which our hostapd runs as.
  With a key present the hotspot never falls back to open: a malformed key or the
  net_mgr route (open hotspots only) refuses to start.
- Why both and not only the password: most users won't set one at install time, and a
  forgotten password locks out the very recovery path the hotspot exists for (adb is then
  the only way back). The button protects every clock with no choice to make; the
  password adds the encryption for those who want it.
- Checked on clock #2 (eng): the hotspot started by hand with a key showed up in clock
  #3's scan as `[WPA2-PSK-CCMP]`; stop clean; clock back on its network. Not yet: a
  phone joining with the password, the live button flow, retail hostapd with a key.

## Back to stock, vendor reset, and the hotspot channel move (2026-11-11, v0.86.0-0.86.1)

- **Back to stock was broken** since the wrappers were written: `/system/chrome/cast_cli`
  (our wrapper) ran under `/sbin/busybox`, which is root-only (0700), and init starts Cast
  as user chrome ("executing /system/chrome/cast_cli failed: Permission denied"). With
  `enabled` removed the clock came up with the vendor daemons but no Cast, and
  input_service waits for /tmp/cast-control before handling any button, so the vendor
  reset hold did nothing. The wrapper now uses /system/bin/sh (ash: `test`, no `[`).
- **Vendor reset** (stock, alarm + Vol- 12 s): input_service sees the long press, Cast
  plays the countdown (amber LEDs one by one, ticking, chime), fdr_notify does the reset
  (strings: `fw_setenv upgrade_step 3`, `--wipe_cache` into /cache/recovery/command,
  recovery boot; we don't call it ourselves; bootloader env is out of scope here).
  Result on clock #3 (eng): /data erased (clockd dir, saved Wi-Fi networks), /system
  wrappers and their .stock originals kept, root adb kept (answers right after the
  reset), firmware string unchanged (`s420-eng 1.56.3.0.3 test-keys`).
- **clockd's hold** (v0.86.0, stockreset.go) is the same 12 s hold: it repairs an old
  wrapper, removes `enabled` and restarts; the vendor reset is then the same hold on
  stock. Verified end to end by the user on clock #3. On stock the clock talks to Google
  at once (several 443 connections, 5228 push) and the eng build's sshd is back on 22.
- **Hotspot channel move:** one radio, so uap0 runs on wlan0's channel. With nothing
  saved the hotspot starts on 6 (2.4 GHz); when the clock joins a 5 GHz network (ch 36)
  the AP follows and every client drops ~4 s, then reassociates (PC kernel log: "Connection
  to AP lost" at the clock's "Wi-Fi connected", reassociated with different capabilities 4
  s later). The setup page used to give up on the first failed poll 8 s after Connect;
  v0.86.1 waits 40 s of failures and explains the move. Untested on a real setup run yet.

## USB: the panel (v0.91.0), the xHCI death, the NLS limit, and the lost stick (2026-11-11)

Building the System > USB panel on clock #3 with the user's 32 GB stick in the
socket. What the live round settled, in order:

- **Switching back to adb with a stick in kills xHCI, reproducibly.** The idpullup
  toggle makes xHCI see a glitch connect: it re-enumerates the stick degraded
  (full-speed instead of high-speed), then "xHCI host not responding to stop endpoint
  command ... HC died; cleaning up". This also explains the boot-time xHCI death seen
  earlier on #3 (boot+1277 s: same full-speed enum, same death): that was a
  switch-back with the stick in, not spontaneous. Recovery is an unbind/bind of
  xhci-hcd.0.auto (verified twice: dead controller, rebind, stick enumerates
  high-speed at once), which is why SetHost(true) always rebinds.
- **The register route back works**: after the write u2p_r0 reads its boot value and
  the pull-downs are off; the gadget side itself could not be checked without a cable
  (open item for the user: cable test after switching back).
- **The kernel has only the default NLS tables.** iocharset=utf8 and codepage=850
  are both EINVAL ("IO charset utf8 not found" in dmesg, no nls modules in /sys/module);
  the vfat `utf8` mount FLAG "succeeds" but the mount is unusable (root dir lists
  empty). So clockd mounts with plain ro,noexec,nosuid,nodev,shortname=mixed: the
  kernel converts FAT names via iso8859-1 and ä/ö show mangled in the listing.
  Documented in the panel's help instead of pretending otherwise.
  CORRECTED 2026-11-12: the `utf8` flag works (ä/ö/å names list and open); the "empty
  root" seen here was the emptied stick, not the flag. See the v0.91.1 section below.
- **Files on the test stick were lost.** At 23:44 the root dir listed five entries
  (two albums, a compilation, aml_autoscript + .boot); at 23:55 the raw FAT root
  (pulled and parsed on the PC) holds one volume label "muzak" and deleted-entry
  markers. Every mount we made was read-only; the only rw mount in that window was
  the vendor mountd's, which the first watcher build unmounted from under it without
  stopping it (its recovery logic is a black box). Not provable, but the fit is exact.
  CORRECTED 2026-11-12: no black box. /sbin/mount_usb_drive.sh starts with
  `rm -r /data/usb` and mounts read-write; see the v0.91.1 section below.
  Consequences: mountd is now stopped on every path that touches the mount
  (SetHost, startup takeover, watcher), and the stick should be recovered on a PC
  (testdisk) if its content matters before it is reused as a test stick.
- **After several xHCI death/rebind cycles the read path lies**: a mounted stick
  listed empty while raw sector reads returned a mix of cached-good and garbage
  pages (dd through the block device rides the page cache; a "random bytes" read at
  a wrong-offset guess and a "deleted entries" read at another both fit whatever
  hypothesis was current). A soft reset (unbind usb-storage + xhci, drop_caches,
  rebind) did NOT clear it; the empty listing was true (the dir really was emptied).
  Lesson recorded: when the controller has died mid-session, distrust reads until a
  reboot or a second reader (the PC) confirms.
- Panel behavior notes: the listing is truthful about an empty drive ("empty"), the
  status line shows device + used/total (statfs), a drive no filesystem fits shows
  its mount error in the panel and is not retried until it changes or the mode is
  switched again (one log line, no 3 s spam).

## USB rework (v0.91.1): what emptied the stick, and the switching glitches (2026-11-12)

Second pass over cmd/clockd/usb.go after the user's report (lost stick, glitchy
switching, "Directory bread failed" in dmesg, nothing happening in the panel).

- **What emptied the stick.** The vendor /sbin/mount_usb_drive.sh (the mountd service)
  begins with `rm -r /data/usb`, then mounts the stick read-write. Started while a
  stick is mounted writable at /data/usb, it deletes the stick's files. That fits the
  evidence exactly (volume label left, every entry a deleted-entry marker). The start
  itself was not found in the session history, so the trigger is inferred; the
  mechanism is in the script. Rule: never start mountd. clockd stops it and mounts
  read-only itself; a read-only mount would have survived the `rm`.
- **The "bread failed" listings were a dead mount.** v0.91.0 ignored a failed unmount
  when switching back to adb (any shell with its cwd in /data/usb makes it fail) and
  flipped the port anyway. The mount stayed, its drive gone; on the next switch to
  host the stick came back as sdb while clockd saw "already mounted read-only" and
  served the dead sda1 mount. The user's dmesg shows just that: `[sdb] Attached`,
  then `FAT-fs (sda1): Directory bread failed`.
- **xHCI no longer dies on the way back.** Unbinding xhci-hcd.0.auto BEFORE the
  register write gives the stick a clean disconnect: 3 full cycles with the stick in,
  every return enumerates high-speed, no "HC died" (v0.91.0: every time). The one
  remaining death is at boot with a stick in (15 s, the port is in device mode then);
  the bind on entering host mode unbinds first, which revives it.
- **vfat `utf8` works.** Names with å/ä/ö list and open as UTF-8. iocharset=/codepage=
  stay EINVAL (no NLS tables), so only the flag is passed.
- **Design now**: /proc/mounts and /sys/class/block are the only state (no cached
  device name). The watcher (3 s, host mode only) clears a mount whose block device
  is gone and mounts what arrives. A busy read-only mount is lazily detached on the
  way to adb (nothing to flush; the holder's reads fail), a busy WRITABLE mount
  (someone else's) refuses the switch. Files() refuses when nothing is mounted.
- Verified on clock #3: 3 cycles through the API (to adb 0.4 s, to host 2.5 s, the
  å/ä folder listed each time); a switch with an adb shell sitting in /data/usb
  (detached, stick back as sdb1 and mounted, no stale listing); pull simulated by
  unbinding xHCI in host mode (mount cleared within a tick, panel says no drive,
  remounted after bind). Not done: a physical pull, the panel in a browser.
- v0.91.2 (same day, user): switching back to adb is refused while a device is on the
  bus (/sys/bus/usb/devices, root hubs and interfaces excluded); the panel greys the
  button out. So the busy-mount detach now only matters for a mount whose drive is gone.
- adb in the LXC does not notice the clock coming back on the cable (no hotplug events
  in the container): lsusb shows 18d1:4e26 and the gadget says CONFIGURED, `adb devices`
  lists it only after `adb kill-server` (then `adb connect` the Wi-Fi clocks again).

## 2026-11-12: FLAC decoder (v0.92.0)

Chose github.com/mewkiz/flac v1.0.14 over writing a decoder: Unlicense (cleanest for the
public repo), pure Go, frame-at-a-time Parse API, and a 2026-08 robustness pass against
corrupt/malicious files (#77/#81). Wrapped as output.DecodeFLAC with the same contract as
DecodeWAV (io.Reader → Buffer of 48 kHz stereo float32), reusing the WAV path's linear
resampler: every consumer (URL playback, USB files, later Sendspin chunks, HA streams)
already funnels through a Buffer, so nothing else changes.

Details that matter later:
- A frame header may carry rate/bits or point back at STREAMINFO (mewkiz leaves the field
  0); fall back to s.Info per frame. Rate/channel changes mid-stream are refused (FLAC
  pins them; only block size varies, and the resampler doesn't care).
- For Sendspin: frame.Parse(stream.r) works on bare frame bytes with no "fLaC" marker or
  STREAMINFO: a chunked FLAC stream (no metadata) can be decoded chunk by chunk, which is
  what advertising flac in PlayerV1Support will need. Bit depth for header-less frames
  comes from the StreamStart message instead.
- >2 channels: keep the front pair (same as the WAV path). Mono duplicated.
- Tests pin exact decode with golden SHA-256 hashes over the full resampled output
  (fixtures: ffmpeg sine-mix tones, 44.1 k/16-bit stereo, 48 k/16-bit mono, 48 k/24-bit
  stereo, ~130 kB total in internal/output/testdata): a resampler change now fails the
  FLAC test too, not just the WAV one.
- Play plumbing refactored: one startMedia(name, fill) owns the buffer, player state and
  the now-playing name (new media_name/media_playing status fields); playMediaURL and
  playMediaFile (USB) are thin wrappers; stopMedia unchanged.

## 2026-11-12: FLAC to HA and Sendspin (v0.93.0)

- HA picks the FIRST listed default (and first announcement) supported format for its
  ffmpeg proxy (homeassistant esphome/media_player.py _get_proxy_url), so flac must
  lead the list; wav stays listed as fallback. Whether HA honors it shows in our log:
  "fetching audio" prints the Content-Type the proxy sends. An ESPHome entry reload in
  HA may be needed for the new format list.
- Sendspin's stream/start carries codec_header (base64 "fLaC"+STREAMINFO): the server
  sends stream metadata once, chunks carry whole frames (sendspin-go's own FLACEncoder
  emits one frame per Encode call). Decoded by output.FLACChunkDecoder: header parsed
  once to pin rate/ch/bps, every frame cross-checked; a frame straddling a chunk
  boundary is buffered (tested down to 1-byte feeds against the file decoder's golden
  hash); CRC failure drops the chunk with a log line.
- maxQueue raised 10 s → 30 s: the server fills the advertised buffer_capacity (1 MB),
  which is ~5.4 s as PCM but up to several times that as FLAC; with the old cap the
  oldest-first trim would have dropped the chunks about to play (the known 5 s failure
  mode). Memory bound: 30 s stereo float32 ≈ 11.5 MB, only reached if the server
  actually streams that far ahead.

## 2026-11-12b: how HA picks the audio format for an ESPHome satellite

Read from HA core (esphome media_player.py, assist_satellite tests) after voice-assist
responses stayed WAV through a full HA restart:

- Media plays and announces with an external media_id: HA rewrites the URL through its
  ffmpeg proxy in the first listed DEFAULT (or ANNOUNCEMENT for announces) supported
  format: /api/esphome/media/<device>/<format>/<rate>/<ch>/<width>/…. Order in
  SupportedFormats decides; flac must be first. go-esphome-device v0.0.17 does send the
  list in ListEntities (domain_mediaplayer.go formats()).
- Voice-assist responses: no proxy at all. HA pulls the TTS format from the device's
  first media player (first ANNOUNCEMENT entry) and hands it to the TTS engine as
  preferred-output options (tts_audio_output → ATTR_PREFERRED_FORMAT etc.). An engine
  that supports the option produces tts_proxy/<token>.flac; one that doesn't gets the
  options POPPED and serves its native format: no ffmpeg conversion on this path. So
  /api/tts_proxy/….wav with type=audio/x-wav means "engine can't do flac", not "clock
  misconfigured". The user's pipeline TTS is a custom Wyoming bridge for fish-s2-pro
  (checking whether the bridge supports format options next).
- Diagnostic: our "fetching audio" log line tells the two paths apart by URL shape
  (tts_proxy/<token>.<ext> = engine output, /api/esphome/media/… = converted proxy).

## 2026-11-12c: voice answers stayed WAV because of the SPEAKER feature flag

Correction to 12b. Read again from HA core dev (esphome/assist_satellite.py, tts/__init__.py):

- clockd advertised VoiceFeatures with FeatureSpeaker (since v0.1.0). In HA that flag means
  the device takes the answer as audio over the API/UDP. run_pipeline then sets
  tts_options to wav / 16000 Hz / 1 ch / 2 bytes on every run, and on TTS_END starts
  _stream_tts_audio (VoiceAssistantAudio messages; "Only WAV audio can be streamed").
  The media player formats are only consulted when the flag is ABSENT
  (`if not (feature_flags & SPEAKER): self._update_tts_format()`).
- clockd plays the URL from TTS_END. It also has an OnTTSAudio handler (onTTSAudio) that
  would play API audio, yet answers were never heard twice: _stream_tts_audio returns
  early when the run is no longer running, and RUN_END follows TTS_END within a
  millisecond in our logs. Not measured further; the handler stays, unused in practice.
- 12b's claim that an engine without format options serves its native format is wrong:
  the TTS manager pops unsupported preferred options and converts with ffmpeg afterwards
  (needs_conversion = final_extension != extension or convert_options). The engine
  (fish-s2-pro Wyoming bridge) does not need to support flac.
- v0.93.1 drops FeatureSpeaker: same advertisement shape as a Voice PE (voice assistant +
  API audio for the mic + timers + announce + start conversation, media player for sound).
  Verified by the user on #2 (two voice turns): tts_proxy/<token>.flac, type=audio/flac,
  410 kB and 1.04 MB answers, streamed while the engine produced them (5.3 s and 20.6 s).
- v0.93.2 log wording: "audio fetched" is now "audio stream ended" (playback starts at
  "fetching audio"; "took" is the sender's time), and TTS_START logs chars=N instead of
  repeating the text INTENT_END already logged as speech.
- Flac health check from the logs the same day: Sendspin flac on #2 and #3 with late=0, no
  "flac chunk dropped", no playback underruns; HA announcement through
  /api/esphome/ffmpeg_proxy/….flac on #3 decoded without error.

## 2026-11-12d: the single speaker plays left + right summed

Test on clock #2 (v0.93.3): four 4 s 1 kHz tones through /api/play (48 kHz stereo WAV,
-10 dBFS): left only, right only, both in phase, both with the right channel inverted.
The user heard three tones and measured the first two at about -58 on a level meter and
the last audible one at -51. Reading: left and right alone are equally loud, in phase is
about 6 dB louder (two equal signals added), the inverted pair cancels to silence. That
is a plain L+R sum somewhere after hw:0,2 (clockd itself writes the two channels
unmixed). Confirmed afterwards: the inverted pair played alone was silent to the user, the
in-phase tone right after it was audible.

Consequences: stereo music loses nothing (only out-of-phase content cancels, as in any
mono mix); no downmix needed in clockd. Mono vs stereo requests to HA or Music Assistant
change bandwidth only, and for flac with identical channels not even that (the mono
voice answers of v0.93.3 came out the same size as the stereo ones). The clock's
`sound ended mic_dbfs` figures varied 13 dB between two runs of the same tone and are no
use for this kind of level comparison.

v0.93.4 went back to stereo for the announcement format since mono gained nothing. Mono
is one field away: `Channels: 1` on the first announcement entry in main.go.

## 2026-11-12e: the amp can be switched off when idle without a pop

The speaker hisses quietly whenever the amp is powered, and clockd powers it once at
start (output.Run: amixer "Dummy Power" on, gpio-453) and never off. Test on clock #2
(v0.93.4), amp toggled by hand over adb while clockd kept writing silence to hw:0,2,
twice three cycles of 5 s off / 5 s on: the hiss is gone while off; switch-off is
silent; switch-on makes a tiny sound audible only with an ear at the speaker (user's
observation). clockd logged no write errors. There is no mute or ramp control on card 0,
the GPIO is the only handle. Not measured: how long after switch-on the amp passes
audio (decides whether the first syllable or chime is clipped).

## 2026-11-12f: clockd v0.94.0 switches the amp off when idle

internal/output: the playback loop counts periods whose mix is digital silence (below
1e-5); after 10 s (ampIdle) it powers the amp down, and the first period with sound
powers it up again. The switching (amixer, 16 ms per call on the clock) runs on its own
goroutine so the real-time loop never waits. clockd keeps writing silence to hw:0,2
throughout, so the echo reference and the loopback are unaffected.

Start-up time, measured on #2 with /api/capture (a 4 s 1 kHz tone played with the amp
off, then on; onset of the raw mics against the speaker reference channel):
- no extra lead (the waking sound is one 80 ms buffer from the speaker): the tone came
  out 33 ms late with the amp off (mic minus ref -143 ms instead of -175 ms). The user
  heard it as a slightly clipped first beep and a swallowed button click.
- ampWarm = 6 (60 ms of silence written in front of the waking sound; tap, reference
  and loopback get those periods too): onset identical with the amp off and on (-175 ms
  both, two runs). So the amp passes audio roughly 100 ms after power-on, and the first
  sound after an idle stretch now plays 60 ms later than otherwise.
Measured on one clock only; on #1 and #3 the user found button clicks after idle whole.
Sendspin starts after idle sound normal to the user (how the player absorbs the 60 ms was
not looked at). Power draw is about the same with the amp off or on (user's
observation): the feature is for the hiss, not for power.

## 2026-11-12g: USB player transport line: mewkiz seek pitfalls (v0.96.0)

- mewkiz/flac Stream.Seek PANICS (interface conversion) on a stream made with flac.New:
  New wraps the reader in a standard bufio.Reader, which has no Seek method, and Seek
  casts stream.r to io.ReadSeeker unconditionally. flac.NewSeek(io.ReadSeeker) is the
  only seekable constructor (bufseekio wrapper, builds a seek table by scanning the file
  when the stream has none: a one-time full read per resume). Never call Seek on a New
  stream; a unit test covers DecodeFrom so a library update that breaks this fails here.
- Seek lands at a frame START at or before the target: up to one block (4096 input
  samples ≈ 93 ms at 44.1 k) replays on resume. Fine for music; documented in the test.
- A resumed decode starts a fresh linear resampler, so its samples differ from a
  continuous decode by interpolation-phase amounts (inaudible; the test allows it).
- Elapsed time: output.Buffer now counts written samples; written minus buffered = what
  reached the speaker (the decode runs up to the buffer length, 5 s, ahead).

## Bluetooth: the radio works; "dead radio" was a bug in our probe (2026-11-12, clock #2)

Re-checked after the USB port turned out not to be charge-only. Result: the BCM4345C5
answers HCI Reset at 115200 every time, takes the vendor .hcd, comes up as hci0 and
scans. The 2026-11-06 section "blocked at step zero" is wrong in its conclusion; the
32 kHz clock was never the problem (pwm-0 runs at 30541 ns = 32.74 kHz and toggles on
GPIOX_20).

What was actually wrong, all in tools/btinit:

1. The read loop never waited. The port was opened with VMIN=0/VTIME=0 and read through
   os.File.Read; a 0-byte read comes back from Go as io.EOF, the loop took that for a
   closed port and returned after microseconds. The chip's answer arrives ~5 ms after
   the command, so every probe printed "silence". The driver's own counters showed it:
   /proc/tty/driver/* rx went up by 7 per Reset while the tool reported nothing.
2. The "wake burst" (16 x 0x01 before the Reset) was a fix for problem 1 and broke the
   chip's packet framing, so the download failed even after the read was fixed. Removed.
3. Rates 1500000 and 3000000 were never those rates: the UART runs from 24 MHz / 3 with
   an integer divider (reg 0xffd24014 after stty: 0x44, 8, 4, 2 for 115200, 921600,
   1500000, 3000000), i.e. 115942, 888889, 1600000, 2666667 baud. The "deterministic
   non-HCI bytes at 1500000" were stale or misframed bytes, not a second listener.
4. attach used N_HCI = 25 (it is 15), passed the value instead of a pointer to TIOCSETD,
   never set HCIUARTSETPROTO, and closed the port right away (the line discipline goes
   with the last close). Now it sets H4 and keeps the port open until killed.
5. The download lacked the Download Minidriver command (0xfc2e) in front of the records.

How it was found: `btinit scope` (new) maps the pin controller from /dev/mem and samples
the GPIOX input register in a loop (~130 ns per sample; pins read back while muxed to
the UART: bit 8 TX, 9 RX, 10 CTS, 11 RTS, 21 bt_rfkill; registers oen 0xff634448, out
0xff63444c, in 0xff634450). A Reset at 115200 showed our 4 bytes on TX with 8.7 us bits
and, 5 ms later, 7 bytes on RX that decode by hand to 04 0e 04 01 03 0c 00.

Verified on clock #2 (eng), by hand:
- Reset -> 040e0401030c00 with and without flow control, also right after an rfkill
  power cycle.
- load: 257 packets, then local version 0e0c01011000090004090f000666, BD_ADDR
  xx:xx:xx:xx:xx:xx (wlan0 MAC + 1), name "BCM4345C5 Ampak_CL1 UART 37.4 MHz BT 5.0
  [Version: 1011.1024]".
- attach -> /sys/class/bluetooth/hci0; `hciconfig hci0 up` UP RUNNING, HCI 5.0;
  `hcitool lescan --duplicates` for 8 s: 510 events, ~20 addresses, among them two
  Ruuvi tags and a TV; ping to HA unaffected during that run.
- Left as found: hci0 down, attach killed, chip powered (rfkill state 1 as at boot).

Not checked: retail clock #1 and clock #3; a long scan (heat, Wi-Fi and audio
coexistence); whether stock bluetoothtbd works (the earlier sniff went through the pty
interposer, which keeps its own port settings, so it proved nothing either way).

Lesson: a "the hardware does not answer" result from our own tool needs one independent
witness (driver rx counters, the pins) before it goes into the notes as a hardware fault.

## clockd Bluetooth proxy, and four kernel-interface traps (2026-11-12, clock #2, v0.98.0)

The proxy went into clockd (cmd/clockd/bluetooth.go, setting bluetooth.proxy, default
off). Working now: bring-up ~7 s, hci0 up, passive LE scan while Home Assistant
subscribes, 1000-1700 advertisements/min forwarded, clean stop/start, web UI card.
Four traps between "btinit works by hand" and "clockd works", all worth remembering:

1. HCIDEVUP passes the device id BY VALUE in the ioctl argument. The kernel handler
   does hci_dev_get(arg) - no copy_from_user. A pointer to the id (the intuitive
   _IOW reading, and what TIOCSETD/HCIUARTSETPROTO next to it take) makes the kernel
   look up a nonsense id and answer ENODEV forever, while sysfs happily shows hci0.
   Proven with btinit's new `sysup` subcommand on a live, attached device: pointer ->
   ENODEV, value -> up.
2. Writes to the HCI raw socket must start with the H4 packet-type byte (0x01 for
   commands). The kernel takes the first byte as the packet type and strips it;
   without it the write returns EINVAL.
3. The hci sysfs entry appears at HCIUARTSETPROTO time and the index is NOT guaranteed
   to be 0. Resolve it like hciconfig does: snapshot /sys/class/bluetooth before the
   attach, then wait for a new hciN and use that index for bind and HCIDEVUP.
4. Closing the last tty fd schedules an asynchronous HANGUP. Reopening quickly and
   attaching the HCI line discipline races it: hci0 registers, the deferred hangup
   unregisters it, and everything after fails. The reliable in-process equivalent of
   the proven two-process btinit flow: close, wait 500 ms+, reopen, and PROVE the port
   talks again (an HCI Reset with answer) before TIOCSETD.
Plus a Go shape bug of mine: a tty read helper that loops until its own deadline adds
that deadline to every round trip - 257 firmware records x up to 2 s = a ~10 minute
"stuck" load that never errors (each record's answer was already in). Return after the
first bounded read (VTIME caps it) and let the caller loop.

What is NOT covered yet: the user's eyes on HA (devices tracking through the clock),
retail clock #1 / clock #3, and long-run coexistence (antenna is shared with Wi-Fi;
the ~30 min agent run showed nothing: Wi-Fi ping fine, no panics).

## Power measurement gotcha: the USB cable is ~1.3 W (2026-11-12, clock #2)

A "+1.3 W since the Bluetooth work" scare (2 -> 3.3 W at the wall meter) was the USB
cable to the LXC: with it plugged in, adbd-over-USB and the dwc3 PHY stay up through
every boot, and unplugging dropped the clock straight back to 1.7 W. The Bluetooth
proxy was fully exonerated (off = hci0 gone; the suspected mic-loop CPU was a red
herring - our WebRTC chain costs ~25 % of a core over the vendor path by design and
is inside the normal 1.7 W idle). Rule for any power reading: unplug the cable first.

## 2026-11-14: wake word false-wake benchmark (6 h evening TV/talk, clock #3)

farec.py recorded the detector input (/api/capture ch 4) on #3 18:46-00:46 (12 x 30 min;
median level -25..-60 dBFS per half hour, TV or talk most of the evening). cmd/wwfile
(WWFLOOR=0.5) over all chunks, table by tools/wwtest/fascore.py. Bursts per 6 h:

    model               cutoff  >=0.5  >=0.8  >=0.9  >=cutoff  max
    hey_mycroft          0.95     59     18      8      4      1.00
    alexa                0.90     21      5      3      3      0.99
    hey_jarvis           0.97     18      5      1      1      0.98
    okay_nabu            0.97      2      0      0      0      0.77
    hey_home_assistant   0.97      1      0      0      0      0.79
    okay_nabu_ml (0.85), choo_choo_homie, hey_glados, hey_luna, okay_computer: none >=0.5

- Offline matches live: the one hey_jarvis hit (chunk 20:16:45 +1230 s = 20:37:15) is the
  false wake #3 logged at 20:37:14 ("I'm actually very well with animal in the car"); #3
  also logged 20 hey_jarvis near misses in the same 6 h.
- "Hey" + 2 syllables is the trouble: mycroft/jarvis/alexa all fire on TV; okay_nabu and the
  longer experimental models stay below 0.8.
- okay_nabu_ml (kahrendt release okay_nabu_20241226.3, 7 languages, cutoff 0.85, the newer
  Voice PE model) scores the user's 10 wwtest clips (1 s silence padded) only 0.44-0.84:
  10/10 misses at its cutoff, vs 0.89-1.00 for the current okay_nabu. Not usable as is.
- Zero false wakes says nothing about the hit rate: okay_computer, hey_home_assistant,
  choo_choo_homie, hey_glados, hey_luna still need the user's voice on the clock.

## 2026-11-14: rain on a tin roof - settled on v1, six dead ends

Goal (user): rain on a tin roof, heard from inside the house, next to "Rain on a felt roof".
Felt roof was accepted at once ("pretty ok"). Tin went through 7 versions, judged by ear by
the user on 60 s previews (seed 5, mono WAV) and on clock #2's small speaker. **v1 shipped.**
Code of every version is in git (noise.go at each commit); previews in the git-ignored
`img/sounds/rain_tin/rain_tin-vN.wav`.

Shared model (noise.go, `roof()`): Poisson drops (size = Exp, cap 4) strike a bank of
two-pole `resonator`s ("panels" x modes, random weight per mode per drop); drop sum low-passed;
plus a "bed" of band-passed pink noise; slow gusts (target every 15-45 s, rate and level).
Layers normalised by `roofNorm` (TestRoofDiag-style RMS run), final gain to RMS 0.1.

| v | commit | change | verdict (user) |
|---|---|---|---|
| 1 | 28cd725 | 3 sheets x 4 plate modes (f0 160-280 Hz, ratios 1/2.32/3.91/5.6, ring 110 ms / (1+0.6k)), 220 drops/s, drops LP 3.2 kHz, bed 250 Hz-2.2 kHz, mix 0.85 drops / 0.55 bed | "the first one is still the best"; has a reverb / "inside a metal tube" feel; "out in the rain" (bed too loud/bright) |
| 2 | a3e79cf | 6 patches, f0 380-700 Hz, rings 35 ms, a 0.5-1.5 ms noise tick (>1.5 kHz) per drop, bed 0.22 & LP 1.2 kHz, 150 drops/s | "much worse... a wooden kalimba on high speed... soft, hollow wooden percussive" |
| 3 | 96b50aa | 1 sheet, 20 dense inharmonic modes log-spread 700 Hz-5.5 kHz, 40-120 ms, tick kept, faint 95-180 Hz thrum, 300 drops/s | "metallic ringing" |
| 4 | 09de45f | felt recipe made brighter: 4 boards x 2 modes 170-750 Hz (9-18 ms), out LP 2.8 kHz, tap >800 Hz, 260 drops/s | "less echoy" but "rain on a tightly bound leather" |
| 5 | aac3f91 | back to v1 with ring 40 ms (was 110) and bed LP 1.2 kHz | "a little better... drops still very hollow" |
| 6 | 87d0d21 | v5 + 2-6 ms noise splash (>500 Hz) per drop, ~2x the ring's RMS; lowest mode at 1/4 | "no hollowness... but rain on plastic" |
| 7 | 12a73e4 | v6 with drops LP 1.8 kHz, splash a bit lower | "still off"; sharp sounds "quite harsh on the small speaker" -> settle on v1 |

Lessons / dead ends:
- **Few tonal modes = an instrument.** Long low rings -> tube/reverb (v1); short higher rings
  -> kalimba/wood (v2); short low boards -> drum skin/leather (v4). The ear hears the pitch
  pattern, not "metal".
- **Dense inharmonic modes = ringing metal (v3)**: physically right for a sheet, but it rings
  like a struck tray, not rain heard through a ceiling.
- **Noise splash removes hollowness (v6)** but makes the drops read as plastic; low-passing
  (v7) did not save it.
- **Sharp transients are harsh on the clock's small speaker** (user, v7): ticks/splashes that
  are fine on headphones get spiky there. Judge on the clock, not only the WAV.
- Muffling the bed (LP ~1.2 kHz) and shorter rings were each heard as improvements (v5).
- Untried ideas if returning: start from v1 and change ONE thing at a time on the clock
  (e.g. only bed LP 1.2-1.5 kHz; only rings 60-70 ms); a recording of real rain on a tin roof
  as a reference (spectrum + drop envelope) instead of physics guesses; drops as filtered
  noise grains with a soft attack (no clicks) through a fixed "ceiling" EQ instead of
  resonators; a slow random detune of the modes so no pitch pattern persists.

## 2026-11-14: unattended run - MP3, update path, setup DNS, SSH keys, DLNA (v0.101-0.106)

Decisions and findings from one night's run on clock #2 (details per version in the CHANGELOG).

- **MP3 (v0.101.0):** decoder github.com/hajimehoshi/go-mp3 (Apache-2.0, pure Go). It
  scans the whole file at open when given a seekable reader, so plain streams get a
  non-seekable wrapper; only resume (DecodeFrom) uses the seekable path. Length/tags come
  from our own header reader (Xing/Info/VBRI, else bitrate x size; ID3v2.2-2.4, ID3v1),
  not from go-mp3. Cost on the A53: ~19 % of one core for 44.1 kHz stereo 128k incl.
  resampling (0.57 s to decode 3 s).
- **Update page (v0.102.0):** release notes travel inside the signed manifest (since 2026-11-17 from
  CHANGELOG.md, before that from the project's task list), so they can't be faked and need no second file. "Go back"
  reuses update/prev and the swap marker: if the power goes mid-move, start.sh's existing
  rollback finishes the same move. Notes are stored with the version they belong to
  (update/notes.json `for`), because a rolled-back update leaves the file behind.
- **Hung updates (v0.102.0):** an update that hangs never exited, so start.sh's
  "3 exits" rule never fired. Now the trial needs a health check (daemon lock within 5 s,
  face loop ticked within 10 s, web UI answers on 127.0.0.1) and clockd exits 3 itself at
  4 min; start.sh additionally kills (-9) a clockd still on trial at 5 min (runs clockd via
  `exec` in the subshell to know its pid). Measured: a deadlocking test build rolled back
  after 3 x ~4 min = 12.5 min. The start.sh guard was only run as an isolated copy.
- **Setup hotspot without dnsmasq (v0.103.0):** busybox 1.38 udhcpd for DHCP (config
  written by setup-ap.sh, `dumpleases` for status) and a ~150-line catch-all DNS inside
  clockd (A -> the clock, AAAA/other -> empty NOERROR, malformed -> dropped). uap0 runs
  next to wlan0, so testing it never cost #2 its Wi-Fi. A real phone lease is untested.
- **SSH keys (v0.105.0):** parsing via golang.org/x/crypto/ssh (BSD, already in the
  module graph). clockd starts/stops dropbear itself (same arguments as start.sh; its
  output to clockd's log, not a pipe, because dropbear forks and would hold the pipe).
  Off = flag file ssh/disabled, honoured by start.sh from the next boot.
- **DLNA (v0.106.0):** own small UPnP stack (internal/dlna) instead of a library: the
  needed surface (SSDP, descriptions, SOAP, GENA, ContentDirectory Browse, AVTransport/
  RenderingControl) is a few hundred lines and keeps clockd dependency-free. Renderer pause
  = a source wrapper that outputs silence and stops reading the buffer, so the HTTP stream
  simply waits (a long pause may make the sender drop the connection - untested). No seek:
  URL playback has no position-based restart yet. GENA callbacks are limited to the
  subscriber's own IP (no relaying NOTIFYs to third hosts).
- **Test environment:** multicast from the build machine (a container) does not reach the LAN (no device
  answers an M-SEARCH from it); the clocks do see LAN multicast (tcpdump on #2 showed #3's
  M-SEARCH and other devices' NOTIFYs). HA discovery of the DLNA devices is therefore still
  unverified.

## Relocked clock running clockd (2026-11-17)

Tested by the user: a clock with clockd installed, locked again with `luna-unlock.py --lock`
(`factory_mode` 1), keeps working. clockd, the web UI and ssh (dropbear, keys only) run
as before, and clockd runs as the same user (root) as on an unlocked clock: it starts from the
Cast wrappers in `/system`, not through adb. The only difference is adb: closed over USB and
over the network (5555). So locking is the recommended last install step; updates then go
through the web UI (signed packages), and anything adb-based needs an unlock first.

## FM radio audio, for nitpickers (2026-11-23, clockd 1.6.23)

Measured offline on synthetic stations through clockd's own code (the RTL-SDR stick at
1.92 MS/s, three half-band stages to 240 kS/s, the polar discriminator, a 151-tap low-pass
to 48 kHz, 50 us de-emphasis; the stream adds a 48 -> 32 kHz resampler). Reproduce:
`MEASURE=1 go test -run Measure -v ./internal/fmradio ./internal/web`.

- **Frequency response** (de-emphasis taken out, 1 kHz = 0 dB): -0.01 dB at 5 kHz, -0.06 dB
  at 15 kHz, -0.11 dB at 16 kHz. With de-emphasis the curve is the standard 50 us one
  (-12.3 dB at 15 kHz), within 0.1 dB.
- **Clock speaker and the 48 kHz path:** that is all. **The stream (32 kHz FLAC/WAV):** the
  resampler is flat to 14.5 kHz (+-0.03 dB), -0.9 dB at 15 kHz, -3.6 dB at 15.5 kHz; what
  would fold back lands 47 dB down at 15 kHz, 77 dB down for the 19 kHz pilot (13 kHz).
  32 kHz is the usual broadcast rate for 15 kHz audio: Nyquist needs more than 30 kHz, and
  the filter needs the gap between 15 and 16 kHz to roll off.
- **Pilot in the audio:** 84 dB below full modulation.
- **Stereo crosstalk:** -57 to -73 dB from 100 Hz to 15 kHz (-53 dB at 16 kHz). Real
  stations are usually the limit here, not the decoder.
- **The 8x decimator:** flat within 0.06 dB to +-100 kHz (-0.75 dB at 110 kHz), images
  115 dB down.

**What the measuring found:** before 1.6.23 the crosstalk was -34 dB at every frequency.
A polar discriminator measures the phase step between two samples, that is the average
frequency over 1/240 000 s: a sinc low-pass on the multiplex signal. Harmless for the audio
(-0.06 dB at 15 kHz), but the stereo difference signal sits on the 38 kHz subcarrier, where
the sinc is down 0.36 dB: L-R came out 4 % weak, which is -34 dB of crosstalk. clockd now
multiplies L-R by x / sin x (x = 2 pi 38 kHz / 240 kHz, about 1.043). The earlier stereo test
built its test signal by sampling the multiplex signal at the sample instants, which skips
that averaging, and so it never saw the droop; the test signal now integrates over each
sample like a real one, and the test asks for 50 dB of separation.

## Where the FM radio code comes from (2026-11-25)

The radio's code went through a provenance audit before its first public release: where
each part came from, and a comparison with librtlsdr and the Linux drivers. Its RTL2832U
setup follows librtlsdr (GPL-2.0-or-later), which is why clockd moved to GPLv3 in 1.7.0.
The record and the rules for new radio code are in [radio-provenance.md](radio-provenance.md).

