# clockd details

The rest of the documentation; the short version is the [README](README.md).

## Which clocks

- **Tested on:**
  - the retail firmware (OPENMASTER 1.52);
  - an engineering build some units shipped with (`s420-eng 1.56.3.0.3`, beta channel).
- Both need unlocking first (**[luna-unlock](https://github.com/pullasuti/luna-unlock)**). `adb shell getprop ro.build.display.id` shows
  which build a clock runs.
- In daily use on three clocks. It's a personal project: expect rough edges.

> [!WARNING]
> **Engineering build:** adbd runs as root, and while the clock is unconfigured its open
> setup hotspot also exposes an SSH server that accepts **root with any password** (tested;
> the build has no account database, and all units share the same SSH host key from the
> firmware). Treat such a clock as a root shell for anyone in range until clockd is
> installed: clockd stops the vendor hotspot and SSH, and the retail firmware refuses to
> run SSH at all.

## Back up the clock first

`satellite/tools/dump-partitions.sh` copies every NAND partition of the clock to the PC:

```sh
satellite/tools/dump-partitions.sh <adb-serial> backup-<clock-name>
```

- **Read-only:** it reads the `/dev/mtd/mtdNro` devices and never writes to flash. It first
  checks that `/proc/mtd` has the CD-4N341Y layout (9 partitions: bootloader, tpl, misc,
  boot_a/b, system_a/b, factory, data) and stops otherwise.
- **Verified:** it copies 8 MiB at a time through the clock's RAM (`/tmp`), compares the
  SHA-256 of each chunk on the clock and on the PC (3 tries), and writes `SHA256SUMS`.
- **Time:** about 2 min 20 s over USB for the whole 500 MiB. Use USB: adb over Wi-Fi tends to
  drop during long transfers.
- **Data partition:** `data` is in use while the clock runs, so its copy is a snapshot of a
  live file system. For a clean copy, remount it read-only first
  (`adb shell mount -o remount,ro /data`; that's how clock #1's backup was made; if it
  reports busy, something still writes there) and restart the clock afterwards.
- **Keep it private:** the dumps contain the clock's serial number, MAC addresses, Wi-Fi
  settings and factory certificates. Don't publish or share them.
- **Inspecting:** the system images unpack with
  [ubi_reader](https://github.com/onekey-sec/ubi_reader) (`ubireader_extract_files`).
- **Restoring** from these images has not been tested and isn't covered here.

## Setup details and tips

- **Name:** a new install names itself after its MAC address (`lenovo-clock-1a2b`,
  "Lenovo Smart Clock 1A2B"), so several clocks don't collide.
- **Wi-Fi:** a clock that already has a network keeps it. The setup page is also at
  `http://192.168.255.249/` on the hotspot; it closes once the clock is connected. Holding
  Vol+ and Vol- for 5 s starts it again.
  - When the clock joins your network, the hotspot moves to the same channel (one radio):
    your phone drops off for a few seconds and reconnects by itself. Keep the page open.
  - After the first boot the clock shows the time about 15 s in.
- **Encryption key:** in the web UI at `http://<clock-ip>/` (Home Assistant tab, after
  logging in with the button), or `adb shell cat /data/local/clockd/api.key`.
- **Voice wizard:** HA plays a test sound on the clock and asks for the wake word.
- **Wake words in HA:** with two wake words active, a freshly added clock shows "No wake
  word" in both of HA's selectors (HA only adopts one). The clock still listens for both; set
  both selectors once, otherwise changing one later turns the other off.
- **Installer options** (`satellite/install-clock.sh`):
  - `-s CLOCK`: which clock; only needed when adb sees more than one device. Over Wi-Fi
    `<clock-ip>:5555` (after `adb connect <clock-ip>:5555`), over USB the serial from
    `adb devices`.
  - `--latest`: download the latest release package and install it.
  - `--release FILE`: install a package you downloaded (a first install or an update, see
    Updating below). Add `--allow-downgrade` to install an older version.
  - `--build`: build from source without asking.
  - `--no-build`, `--no-start`, `--yes`.
  - `--hotspot-password`: give the setup hotspot a WPA2 password (asked for, or
    `$HOTSPOT_PASSWORD`). Only the derived key goes to the clock, so keep the password.
    `--no-hotspot-password` makes the hotspot open again.
  - `$WIFI_PASSWORD`: the Wi-Fi password without asking (empty = open network). Only the
    derived WPA key is sent to the clock.
- **Updating:** with a signed release package.
  - Web UI: System > Update. Choose the file, check what changes, Install and restart.
  - Online check: "Check for updates online" on the same page (off by default). The clock
    asks GitHub once a day for the latest release and offers to download it; installing
    stays your click.
  - Home Assistant: with the online check on, "Offer updates in Home Assistant" (off by
    default) shows a newer release on the clock's Firmware entity, and Install there runs
    the same download and checks. Keep it off while adding the clock to Home Assistant: the
    voice assistant setup installs any update it sees.
  - Over adb: `install-clock.sh -s <clock-ip>:5555 --latest`, or `--release
    clockd-<version>-arm64.tar.gz`.
  - Safety: the installed clockd checks the signature and every file, keeps the files it
    replaces, and switches back by itself if the new version exits three times within its
    first minute. Older versions are refused unless `--allow-downgrade` is given.
  - From source: run `install-clock.sh --build` again. For clockd alone:
    `adb push satellite/build/clockd-arm64 /data/local/clockd/clockd.new`, then
    `adb shell /sbin/busybox sh /data/local/clockd/restart.sh`.
  - Making packages: `satellite/release.sh` (needs the signing key, see "Release packages"
    in [docs/notes.md](docs/notes.md)). The notes on the Update page come from
    [CHANGELOG.md](CHANGELOG.md).
- **Back to stock** (by adb):
  - `adb shell rm /data/local/clockd/enabled`, then power-cycle: Cast boots.
  - `uninstall.sh` also removes the wrappers.
- **Back to stock without a PC:** hold the alarm button and Vol- together for 12 s. The
  LEDs turn amber one by one and the face counts down; letting go cancels. At the end a chime plays and the clock restarts into the stock software. Nothing is
  deleted yet. The stock software talks to Google again straight away (and on the
  engineering build its SSH server is back on port 22).
- **If clockd keeps crashing:** after five failed starts in a row (each within a minute)
  the clock goes into rescue mode until the next power cycle: "Err" on the face, dim red
  LEDs, nothing else running. The 12 s hold works there too (silent: no pips or chime).
  A power cycle tries clockd again. The log has the reason: `start.sh: clockd failed`.
- **Factory reset:** on the stock software, hold the alarm button and Vol- for 12 s again:
  the vendor's own reset (amber LEDs, ticking tune, chime, restart). It erases `/data`
  (clockd's settings and keys, saved Wi-Fi networks) but not the wrappers in `/system` or
  root adb; run `install-clock.sh` to come back.

## Locking the clock again

Unlocking leaves adb open: root over USB and on TCP port 5555, without authentication.
Once clockd runs, the clock no longer needs it day to day, so you can lock the bootloader
again with the same tool:

```sh
python3 luna-unlock.py --lock
```

Before you lock:
- **Set up ssh:** add your public key in the web UI (System › Network & access › SSH)
  and check that `ssh root@<clock-ip>` works. ssh is keys only; there is no password login.
- **Know your way back:** `luna-unlock.py --unlock` opens adb again. Locking only changes
  one U-Boot setting (`factory_mode`); nothing is flashed or erased.

On a locked clock (tested):
- clockd, the web UI (login by button) and ssh keep working; clockd runs with the same
  rights as before.
- adb is off, over USB and over the network.
- Updates go through the web UI (System › Update, signed packages); going back to stock
  works with the button hold.
- Anything that uses adb needs an unlock first: `install-clock.sh` (fresh installs and
  `--release` from the PC), `dump-partitions.sh` and the optional debugging tools.

## Features

**Voice satellite** over the ESPHome native API (encrypted, Noise PSK); HA discovers it
over mDNS like any ESPHome device.
- On-device wake words (microWakeWord), up to two active; a "Stop" word while it plays.
- Several clocks in one room: the one that heard the wake word best answers (Voice ›
  Nearest clock answers, on by default); the others stay quiet.
- Microphone: the clock's raw microphones through WebRTC audio processing (echo cancelling
  only while the clock plays, noise suppression, gain) and a learned two-mic echo null. There
  is no choice to make: if it fails or isn't installed, the clock goes back to the vendor's
  processing by itself and says so on the Microphone page. Why this one, and what was
  measured: [docs/microphone.md](docs/microphone.md).
- Wake sound, software mute (the hardware mute switch always wins), microphone gain with
  a live meter, a wake word history with near misses.
- HA entities: mute, night light, LEDs, wake sound, Restart / Restart clockd buttons, a
  firmware entity showing the version.

**FM radio** (experimental) from an RTL-SDR USB stick, tuned by the clock itself; no
internet or Home Assistant needed:
- Supported sticks: RTL2832U with an R820T or R820T2 tuner (USB 0bda:2838 or 0bda:2832),
  the common cheap kind. Tested with one generic RTL2838UHIDIR stick. Other tuners
  (RTL-SDR Blog V4 / R828D, E4000, FC0013 and others) are not supported yet: the Player
  then says the stick's tuner is not supported. Sticks without a temperature-stable crystal may sit a few kHz
  off a station; this is untested.
- Audio › Player: station search, a band dial, six favourites, stereo and station names
  (RDS). Choose where it plays first (Clock speaker, This device, or both), then a station.
  On both, the browser runs a few seconds behind the clock (the stream is buffered).
  On This device the phone's lock screen and media keys show the station and can
  pause it or step to the next one.
- Home Assistant: an FM radio dropdown (the favourites, or every station found when there
  are none) and `fm:<MHz>` as a media id (e.g. `fm:91.2`) for play media.
- A stream link for other players on your network (Home Assistant, Music Assistant, VLC):
  FLAC at 32 kHz, about 0.8 Mbit/s; `.wav` instead of `.flac` for players without FLAC.
- Wake up to the radio: an alarm's sound can be a station (the timer sound if the stick
  is missing).
- Audio › FM radio: Region (the band and de-emphasis: Europe and most of the world, the
  Americas and South Korea, or Japan) and Reception (Weak gives the tuner more gain for
  far stations, Strong less when a transmitter nearby makes stations bleed together).
- The stick goes in the USB socket with System › USB on "USB drive" (a hub is untested).
  On an unlocked clock a restart switches the socket back to adb, so switch it again
  after a power cut; a locked clock stays on USB drive.

**Music from a USB drive:** with System › USB on "USB drive" (FAT or ext2-4, mounted
read-only), Audio › Player › USB drive is a music library: albums with their covers,
artists and songs from the files' tags, and a filter across all three. The clock reads the
tags in the background when the drive goes in and keeps the index, so the next time only
new or changed files are read; untagged files are sorted in by their folder and file
names ("Artist - Album (2006)/01. Song.flac"). A song plays with the rest of its album
(or of the list it was picked from) after it; pause, previous/next, shuffle and repeat.
Folders shows the drive as it is. FLAC, MP3 and WAV. It plays where Plays on says:
- Clock speaker: the clock plays the files. A Clock volume slider sits right there (the
  same volume as Audio › Volume and Home Assistant's media player).
- This device: your browser fetches the files from the clock and plays them itself,
  with seeking and lock-screen controls. It has its own queue, so the clock can play
  something else meanwhile, and closing the page stops it.
- Both: two separate players, **not in sync**: each starts on its own and drifts apart.
  This is for listening in another room, not for one sound from two speakers (for
  that, use Music Assistant's synchronised playback).

The DLNA card can share the drive with Home Assistant's media browser and DLNA apps.

The Player also shows whatever else the clock plays (Music Assistant, Home Assistant,
DLNA) with a Stop, and the Audio tab gets a dot while something plays.

**Music Assistant speaker (Sendspin):** discovered automatically, synchronised playback,
volume and mute from Music Assistant, "artist - title" on the face.

**Clock face** (the TM1652 segment display, driven directly): time and weekday, outdoor
temperature and condition icons, animated eyes while listening, thinking and speaking,
scrolled text from the web UI or HA, login codes, auto brightness from the light sensor.

**Lights and buttons:** status LEDs with per-state colours and animations, the night
light, volume buttons (hold to repeat), action button (talk / stop), mute switch.

**Alarms** (the clock's own, kept on the clock and working without Home Assistant once the
time is known):
- Once, every day, weekdays, weekends or chosen days; set in the web UI or with the
  clock's buttons as on the stock firmware.
- The alarm button stops one; any other button snoozes it (5-20 minutes).
- Ten sounds (beeps and short tunes, all generated on the clock), an FM station when
  an RTL-SDR stick is plugged in, or a song from the USB drive (Sound › Music from the
  USB drive…, then find the song). The song repeats until you stop the alarm. If the
  drive or the song is missing, or the song doesn't start within 3 seconds, the alarm
  rings with the timer sound instead, so it always rings.
- Optional sunrise: the night light comes up before the alarm when the room is dark.
- Home Assistant sees the next alarm and can set, switch, delete and list alarms
  ([actions](#home-assistant-actions)).

**Notifications from Home Assistant:** text scrolled on the face and a colour on the
status LEDs ([actions](#home-assistant-actions)). Alarms and voice turns show over an LED
notification; it comes back after them if its time isn't up.

**Do not disturb:** by hand, while night mode is on or between own times; silences the
hourly chime, the wake sound, Home Assistant's announcements and the startup sound, never
alarms, timers or answers. A switch in HA.

**Night schedule:** fixed times or sunset/sunrise from HA; dims or turns off the face and
fades the night light; a switch in HA.

**Web UI** on port 80 (Status, Display, Audio, Home Assistant, System): every setting
applies instantly; Wi-Fi scan and switching with fallback; a clock picker for your other
clocks; a readable live log; login by button (a 4-digit code on the face, approve with the
action button); colour themes.

## Home Assistant actions

The clock offers these actions to Home Assistant (Developer tools › Actions, scripts,
automations). Their names start with `esphome.<clock>_`, where `<clock>` is the clock's
ESPHome device name (e.g. `lenovo_clock_1a2b`); it can differ from its entity names, so
look them up in Developer tools › Actions. HA doesn't show an action's error: a refused
call (a misspelt colour, an unknown alarm) is logged on the clock (System › Logs).

| Action | Does |
|---|---|
| `_show_text`, `_clear_text` | scroll a message across the face; drop the messages |
| `_led_notify`, `_led_clear` | a colour on the status LEDs; end it early |
| `_set_alarm`, `_switch_alarm`, `_delete_alarm` | add, switch on/off or remove one of the clock's alarms |
| `_list_alarms` | the alarms and the next one, as a response |

### Text on the face

`_show_text` scrolls a message across the four big digits.

| Field | Values |
|---|---|
| `text` | the message, drawn as closely as seven segments allow; accents are dropped (ä → a), punctuation and unknown characters become a gap |
| `repeat` | how many times it scrolls, 1-20 (0 = once) |

Messages queue (up to 10; the oldest goes first when full) and play one after another at
the scroll speed set in Display › Text messages, where a preview shows how a text reads on
the digits. `_clear_text` drops the current and queued messages.

```yaml
action: esphome.<clock>_show_text
data:
  text: Washer done
  repeat: 2
```

### LED notifications

`_led_notify` shows a colour on all four status LEDs, at the Status LEDs brightness set in
the web UI. A new notification replaces the one showing; `_led_clear` ends it early.

| Field | Values |
|---|---|
| `color` | `red`, `orange`, `yellow`, `green`, `cyan`, `blue`, `purple`, `pink`, `white`, or `#rrggbb` |
| `seconds` | how long it shows; 0 = 10 s, at most 3600 |
| `style` | `pulse` (slow breathing, 2 s; also when left empty), `blink` (0.5 s on / off) or `solid` |

```yaml
action: esphome.<clock>_led_notify
data:
  color: orange
  seconds: 20
  style: blink
```

### Alarms

| Action | Fields |
|---|---|
| `_set_alarm` | `time` `HH:MM` (24 h); `days` `once` (or empty), `daily`, `weekdays`, `weekends` or day names such as `mon,wed,fri`; `label` (optional, a name to find it by). A new alarm is on, with the default sound and fade |
| `_switch_alarm` | `alarm` its label or `HH:MM`; `on` true or false |
| `_delete_alarm` | `alarm` its label or `HH:MM` |
| `_list_alarms` | none; returns `alarms` (each with `id`, `time`, `on`, and when set `days` as numbers 0 = Sunday to 6 = Saturday, `label`, `sound`, `sunrise`, `fade`) and `next` (`at` as a full date and time, `time`, `label`; empty when no alarm is on) |

```yaml
action: esphome.<clock>_set_alarm
data:
  time: "06:45"
  days: weekdays
  label: work
```

`_list_alarms` answers with data: in a script, add `response_variable: alarms` to the call
and read `alarms.next.at`. Sound, fade and snooze length are set in the web UI or with the
clock's buttons.

## How it works

```
Home Assistant ──ESPHome API :6053──┐
Music Assistant ──Sendspin  :8928──┤
Browser ─────────HTTP        :80 ──┤
                                    clockd ── display (TM1652 on ttyS2/ttyS3)
                                       │   ── LEDs (SN3112), night light, buttons, light sensor
                                       │   ── speaker (ALSA hw:0,2, own mixer)
                                       └───── mic: raw PDM mics → apmd (WebRTC) → wake word
```

The Cast software is not removed. Two small wrappers replace `/system/bin/cast_installer`
and `/system/chrome/cast_cli` (originals kept as `*.stock`). When
`/data/local/clockd/enabled` exists, they start `start.sh` instead of Cast; `start.sh`
stops the vendor daemons clockd doesn't need and supervises clockd. Without the flag file
the clock boots stock.

## Local first: what changes on the clock

What clockd changes compared with the stock Google Cast / Lenovo software, and what it
doesn't (yet). Nothing in `/system` is deleted: vendor parts are stopped at boot, several
can be brought back with a flag file in `/data/local/clockd/` (`start.sh` lists them), and
the stock software returns completely with the button hold (see Back to stock).

**Stopped vendor software**

| Stock | What it did | Now |
|---|---|---|
| Google Cast (`cast_shell`, `process_manager`, `update_engine`, `crash_uploader`) | the assistant, Cast receiver, Google OTA updates, crash reports | never started: small wrappers start clockd instead |
| `ota_service` | Lenovo/Google firmware updates | stopped; updates are signed clockd packages you choose to install |
| `collectd` | metrics collection | stopped |
| `sntpd` | time sync from the internet | stopped; time comes from Home Assistant (or an NTP server you name) |
| `net_mgr` | Wi-Fi manager with Google connectivity checks (`generate_204`) and OTA URLs | stopped; clockd talks to `wpa_supplicant` itself and runs the setup hotspot |
| `dhcpcd` | DHCP; put Google's DNS (8.8.8.8) *before* your router's and sent the fixed name "lenovo"/"audiocast" | replaced by busybox `udhcpc`: only your router's DNS, the clock's own name |
| `lenovo_alarm`, `lenovo_status`, `panel_service`, `input_service`, `mute_service`, `wifi_display.sh` | the stock clock face, buttons, LEDs, status | replaced by clockd |
| `sshd` (engineering build) | root login with **any** password, a host key shared by all units | stopped; ssh is our dropbear with keys only |
| `bluetoothtbd`, `iot_audio_capture`, `mountd`, `external_control`, `fdr_notify`, ... | Cast's Bluetooth, the vendor microphone processing, USB auto-mount and more | stopped; clockd runs Bluetooth and the microphone itself |

What is left running: clockd and its helpers (`apmd` for the microphone, `udhcpc`,
`dropbear`), and the system basics `wpa_supplicant`, `adbd`, `logd`, `servicemanager`,
`watchdog`, `ueventd`.

**Network**

- **Listening:** the web UI (80), the ESPHome API (6053, encrypted), Music Assistant /
  Sendspin (8928), mDNS (5353), the wake word vote between clocks (UDP 6054, only from
  clocks found by mDNS), ssh (22, only with keys), and adb (5555, while the clock is
  unlocked). Stock had the Cast ports (8008, 8009, 8443, 10001 and more). DLNA is off
  unless you switch it on. During setup only: DHCP and DNS on the setup hotspot, behind a
  firewall that allows nothing else.
- **Outgoing:** to your own network only: Home Assistant, Music Assistant, your router
  (DHCP, DNS). Time, weather and voice go through Home Assistant; whether Home Assistant
  itself uses cloud services for speech is its own setting. The one optional exception is
  the update check (System › Update, off by default), which asks GitHub for new versions.
- **Web UI:** everything is in the clock (icons, fonts, scripts); the page loads nothing
  from the internet. New browsers are approved with the button on the clock.
- **Updates:** signed packages only, checked file by file before anything changes, with
  automatic rollback. You install them (web UI or adb); nothing updates by itself.

**Not done yet (be aware)**

- **The first seconds after boot:** init starts some vendor daemons before clockd's
  `start.sh` stops them. `net_mgr` has been seen making an HTTP time request to
  `clients1.google.com` in that window. Stopping it earlier needs a change to the boot image,
  which this project doesn't touch. For a hard guarantee, block the clock's internet access on
  your router (it needs none).
- **No outgoing firewall** on the clock itself yet (the kernel supports one; it's planned).
- **adb on port 5555** is root without a password while the clock is unlocked: lock it again
  once clockd runs (see [Locking the clock again](#locking-the-clock-again)).
- **The web UI is plain HTTP** on your network.

## Building and the install package

Most installs don't need this: a release package (see the README's Quick start) has
clockd and every helper, built and signed. To build from source, run
`satellite/install-clock.sh --build`: it builds clockd and installs it with
whatever helpers are in `satellite/build/` (table below). A plain build has none of them:
the clock then has no setup hotspot, no ssh and the vendor's microphone processing, so
save a Wi-Fi network with `--wifi`, or build the helpers first.

Your own builds run on your clock: `install-clock.sh --build` installs them over adb, and
on a clock with ssh you can copy a build to `/data/local/clockd/clockd.new` and run
`restart.sh`. Only the in-app update checks the release signature; that is for our
packages, it never stops a build of your own.

Go 1.21 or newer: the build switches to the version `satellite/go.mod` names (1.27.3) and
downloads it on first use (`GOTOOLCHAIN=auto`; `install-clock.sh` sets it, since some
distributions default to `local`). Pure Go, no cgo; the output is a static arm64
binary:

```sh
cd satellite
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o build/clockd-arm64 ./cmd/clockd
```

The microWakeWord models are in `satellite/models/` (`.tflite` and `.json`):
- `okay_nabu` (on by default), `hey_jarvis` and `hey_mycroft` from
  [esphome/micro-wake-word-models](https://github.com/esphome/micro-wake-word-models)
  (Apache-2.0)
- `hey_home_assistant` and `okay_computer` from the same repository's `experiments/`
  (lightly trained: expect them to need a clear voice)
- `stop` (ends a timer or an answer) from the kahrendt/microWakeWord "stop" release (Apache-2.0)

More models go into the same folder before a source build (`install-clock.sh --build`), or
are added later from the web UI (Voice › More wake words), which browses the ESPHome
collection and [Tater Totterson's Tater-Wake-Words](https://github.com/TaterTotterson/Tater-Wake-Words)
(about 900 community phrases, MIT).

By hand, `install-clock.sh` does:

```sh
cd satellite
device/stage.sh                  # collects clockd, the wake word models and the helpers
adb push build/clockd-device /data/local/tmp/clockd-device
adb shell /sbin/busybox sh /data/local/tmp/clockd-device/install.sh
```

Optional helpers, built into `satellite/build/`:

| Helper | Built with | Without it |
|---|---|---|
| `bin/iptables` | `tools/firewall/build-iptables.sh` | no setup hotspot (it never runs without its firewall) |
| `aec/apmd` | `tools/aec/build-apmd.sh` | the WebRTC microphone falls back to the original one |
| `bin/busybox`, `bin/dropbearmulti` | `tools/toolbox/build.sh` | no current shell tools; no ssh (adb only) |
| `tools/bin/strace`, `tools/bin/tcpdump` (not in the package; `install-clock.sh --tools`) | `tools/toolbox/build.sh` | debugging over adb/ssh only with the vendor tools |

ssh (dropbear, root, keys only, port 22) starts at boot when
`/data/local/clockd/ssh/authorized_keys` exists on the clock; there is no password login.
Licence texts of the built tools: `satellite/tools/toolbox/licenses/`.

`install.sh` can be run again at any time: it never touches settings, keys or wake word
models already on the clock (missing models are added).

## Files on the clock

All under `/data/local/clockd/`:
- `clockd`, `start.sh`, `restart.sh`, `setup-ap.sh`, `models/`, `aec/`, `bin/`
- `update/`: the last update's replaced files (`prev/`) and its state (`trial`, `last`,
  `failed`)
- `config.json`: settings (written by the web UI)
- `state.json`: volume, mute, name, time zone and similar runtime state
- `api.key`: the ESPHome encryption key
- `sessions.json`: approved browsers, stored as hashes
- `enabled`: the boot flag
- `user/`: yours (see Your own programs); clockd and updates never touch it

The log is `/tmp/clockd.log` (in RAM, rotated to `.1` at 2 MB).

## Your own programs

`/data/local/clockd/user/boot.sh`, if it exists, runs once per boot (put it there over ssh
or adb):
- as root, with busybox sh, in `user/`, in the background: clockd doesn't wait for it;
- early: wait for the network yourself if you need it;
- its output goes to `/tmp/clockd.log`, between `running user/boot.sh` and `exited with N`;
- updates, rollbacks and reinstalls never touch `user/`; a factory reset erases it.

Keep the programs in `user/` too: static Linux binaries for arm64 (Go, or C built against
musl), not the clock's own libraries. Keep them light: the microphone needs the CPU.

To skip it: `touch /data/local/clockd/user/disabled`. If clockd keeps crashing after the
script ran, the clock goes into rescue mode (see Back to stock) and creates that file
itself; delete it to run the script again. The 12 s hold works in rescue mode, so a clock
without ssh or adb can still go back to the stock software; clockd comes back with an
unlock and `install-clock.sh`.

## Repository layout

| Path | What |
|---|---|
| `satellite/cmd/clockd/` | the daemon: voice pipeline, face, LEDs, buttons, night schedule, web backend |
| `satellite/internal/` | packages: `display`, `output` (mixer), `capture`, `localmic`, `wake`, `web` (API + embedded UI), `wifi`, `sendspin`, `config`, `haapi`, `hw`, `timesync`, `release` (signed packages, install engine) |
| `satellite/device/` | `stage.sh` (PC) and the on-device scripts: `install.sh`, `uninstall.sh`, `start.sh`, `restart.sh`, `setup-ap.sh`, wrappers in `system/` |
| `satellite/release.sh` | build, stage and sign a release package (`tools/release`: keygen, pack, verify) |
| `satellite/tools/` | test rigs (wake word, echo checks), helper builds, `facesvg` (renders clock face frames as an animated SVG: `go run ./tools/facesvg \| node tools/facesvg/render.mjs > face.svg`) |
| `docs/` | README pictures: logo, face animation, web UI screenshots (`screens/`), UI design rules, the microphone write-up |
| `README.md`, `DETAILS.md` | the short introduction; everything else |
| `docs/notes.md` | engineering notes: measurements, dead ends and why things are done this way, dated |
| `CHANGELOG.md` | what changed in each version (also the Update page's release notes) |

## Security notes

This is a home-LAN device for now:
- The web UI is plain HTTP. Everything except the login page needs a session; a new
  browser is approved with the button on the clock, from a logged-in browser, from Home
  Assistant ("Approve web login"), or with an optional password.
- The setup hotspot is open unless the installer set a password for it
  (`--hotspot-password`), and firewalled: only DHCP, DNS and the setup page are reachable
  on it, and it closes once the clock is connected. The setup page works only after the
  action button on the clock was held. On an open hotspot the Wi-Fi password typed on the
  page crosses the air unencrypted; set a hotspot password if that matters to you.
- Internet: none by default. The optional update check (System > Update) talks to GitHub
  over HTTPS (`api.github.com` once a day, and the package download from GitHub when you
  ask for it); the package's signature is what makes it trusted, not the download.

> [!CAUTION]
> adbd on port 5555 is unauthenticated root while the clock is unlocked. Keep the clock on
> a trusted network or VLAN, or lock it again (see
> [Locking the clock again](#locking-the-clock-again)).

## Credits

clockd is licensed under the GNU GPL version 3 or later (releases up to 1.0.0: Apache-2.0).

- FM radio: the RTL2832U setup follows [librtlsdr](https://osmocom.org/projects/rtl-sdr)
  (osmocom rtl-sdr, GPL-2.0-or-later) by Steve Markgraf, Dimitri Stolnikov and
  contributors. The R820T tuner driver is written from Rafael Micro's R820T2 register
  description. Where the code came from: `docs/radio-provenance.md`.
- Icons: [Lucide](https://lucide.dev) (ISC, `satellite/internal/web/LICENSE-lucide.txt`),
  embedded so nothing loads from the internet.
- [webrtc-audio-processing](https://gitlab.freedesktop.org/pulseaudio/webrtc-audio-processing)
  (BSD-3), [microWakeWord](https://github.com/kahrendt/microWakeWord) and the
  [ESPHome wake word models](https://github.com/esphome/micro-wake-word-models) (Apache-2.0).
- The web UI's wake word browser lists
  [Tater-Wake-Words](https://github.com/TaterTotterson/Tater-Wake-Words) by Tater Totterson
  (MIT); none of them ship with clockd.
- Built from unmodified upstream sources with the build scripts in `satellite/tools/`
  (exact versions, download URLs and checksums there): [BusyBox](https://busybox.net) 1.38.0
  (GPL-2.0), [iptables](https://www.netfilter.org/projects/iptables/) 1.8.10 (GPL-2.0),
  [Dropbear](https://matt.ucc.asn.au/dropbear/dropbear.html) 2026.94 (MIT; our compile-time
  options in `tools/toolbox/patches/dropbear-localoptions.h`), and the optional tools
  [strace](https://strace.io) 6.19 (LGPL-2.1+), [tcpdump](https://www.tcpdump.org) 4.99.7 and
  libpcap 1.10.7 (BSD).
