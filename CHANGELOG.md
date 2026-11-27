# Changelog

What changed in each clockd version, newest first.

Each version gets a `## X.Y.Z (YYYY-MM-DD)` heading. Its first paragraph (or first list item)
is the line the clock's System > Update page shows for that version: one plain sentence,
at most 140 characters. Anything below it is detail for readers of this file.
`release.sh` refuses to build a package without an entry for its version.

## 1.7.0 (2026-11-28)

A new Player with a USB music library and FM radio, songs and radio for alarms, and clockd is now GPL-3.0-or-later.

- **Licence:** clockd is now licensed under the GNU GPL v3 or later (1.0.0 was Apache-2.0).
  The FM radio's stick setup follows librtlsdr (GPL-2.0-or-later) and is credited to it.
- **Player (Audio tab):** one place for music from the USB drive and the FM radio. It plays
  on the Clock speaker, on This device (your browser plays it itself, with cover art and
  lock-screen controls), or both (two separate players, not in sync). A clock volume
  slider sits right there.
- **USB music library:** albums with covers, artists, songs and a filter, built from the
  tags; folders stay as a view. A song plays with the rest of its album after it.
- **FM radio (experimental):** from an RTL-SDR stick (RTL2832U with an R820T or R820T2
  tuner) in the USB socket; no internet or Home Assistant needed. Station search, stereo,
  station names (RDS), favourites, region and reception settings. Without a stick, the
  Player says what you need.
- **Radio on your network:** a secret FLAC stream link for Home Assistant, Music Assistant
  or a browser. Home Assistant gets an FM radio list of your favourites and can play
  `fm:91.2`.
- **Alarms:** wake up to an FM station or a song from the USB drive (the timer sound rings
  if the stick, drive or song is missing); fade in over 1-3 minutes; skip a repeating
  alarm's next ring; press a button again right after snoozing for a longer snooze;
  cancel a snooze.
- **Do not disturb:** silences the chime, wake sound, announcements and startup sound by
  schedule or switch; alarms still ring.
- **LED notifications** from Home Assistant: a colour on the status LEDs for a while,
  pulsing, blinking or steady.
- **Nearest clock answers:** with several clocks in earshot, the one that heard you best
  answers; the others stay quiet (Voice › Nearest clock, UDP 6054 on the local network).
- **Removed:** the `tools/rtlprobe` test program is no longer part of the source.

## 1.0.0 (2026-11-20)

The first public release: the Lenovo Smart Clock Essential as a local Home Assistant voice satellite and alarm clock.

clockd replaces the clock's Cast software with one local program. Nothing from the stock
system is deleted, and going back is a 12 s button hold.

- **Voice:** wake words on the clock, Assist over the ESPHome API, timers, announcements,
  a stop word; echo cancellation and the two-microphone null run on the clock.
- **Alarm clock:** alarms kept on the clock and set from its buttons, the web UI or Home
  Assistant (also by voice, with the blueprint in docs/blueprints); snooze, hourly chime,
  sleep sounds.
- **Clock face:** time, weekday and weather from Home Assistant, auto brightness, little
  eyes while it listens, a startup animation.
- **Night schedule:** fades the face and the night light, follows the room light if you
  like.
- **Music:** a Music Assistant (Sendspin) speaker, a Bluetooth proxy for Home Assistant,
  and a player for music on a USB stick.
- **Web UI:** setup, settings, logs and updates in the browser, with sixteen themes.
- **Local:** no Google or Lenovo services and no internet needed. The only online feature,
  the update check, is off until you turn it on.
- **Updates:** signed packages, installed from the web UI or, if you opt in, from Home
  Assistant's firmware entity.

Getting started: unlock the clock with luna-unlock over a USB-A to USB-A cable, then run
the installer (README, Quick start). Tested on the retail and the engineering firmware.

## 0.115.1 (2026-11-20)

The clock shows "usb" when you plug in a USB stick in drive mode, and "err" if it can't read the stick.

## 0.114.8 (2026-11-20)

New: a user boot script, a crash rescue mode, smoother night fades and friendlier defaults for new installs.

- Your own script at /data/local/clockd/user/boot.sh runs once at boot; updates leave the
  user folder alone.
- If clockd keeps crashing, the clock shows Err and the 12 s hold back to the stock
  software still works.
- Night mode: the clock face fades at the night's start and end as slowly as the night
  light (default fade 2 minutes), and the night light returns to its daytime level when
  night mode ends instead of switching off.
- The night light follows speech more clearly, also at full brightness.
- The back-to-stock hold counts down on the face and ends with the six radio time pips.
- On a locked clock (no adb) the USB socket starts on the USB drive, and the panel no
  longer offers adb.
- New installs start with the startup animation on, white status LEDs, the night light
  following the voice, a blinking colon and automatic display brightness.
- Home Assistant: a blueprint to set, list, delete and switch the clocks' alarms by voice
  (docs/blueprints), and FAQ tips for timers and alarms with a language model.

## 0.112.1 (2026-11-19)

Fixed: a first setup no longer logs the browser out when the clock gets the real time from Home Assistant.

## 0.112.0 (2026-11-19)

New: updates can be installed from Home Assistant (opt-in), and the web UI is smoother on phones.

- Home Assistant: with "Check for updates online" on, "Offer updates in Home Assistant"
  (System > Update, off by default) shows a newer release on the clock's Firmware entity,
  and Install there runs the same download and checks as the web UI.
- The web UI is smoother on phones: live values update in place instead of being redrawn,
  and glow is off by default on touch screens.
- First setup: the weather step explains how to set it without the Home Assistant API, and
  the setup page counts down to the hotspot closing.

## 0.111.0 (2026-11-18)

New: an optional startup animation, and night mode's dimmed face can follow the room light.

- Startup animation: the original luna teaser plays once when the clock starts (Display >
  Clock face, off by default). It stays dark when night mode has the display switched off.
- Night mode's dimmed face can follow the room light; the slider then sets the most it gets
  (Night schedule > Clock face).
- Night mode's dimmed brightness now applies as set, also when it is brighter than the
  normal display brightness.
- Brightness 1 is now the dimmest lit face instead of off. Every brightness level is one
  step brighter than before; 16 is unchanged.
- The chime preview plays the current hour.
- Night mode no longer flashes the time when the clock starts.

## 0.109.1 (2026-11-17)

The update package now really includes Hey Mycroft, Hey Home Assistant and Okay Computer (0.109.0's lacked them).

## 0.109.0 (2026-11-17)

New built-in wake words from ESPHome: Hey Mycroft, Hey Home Assistant and Okay Computer; Hey Luna is no longer included.

All included wake words now come from ESPHome's micro-wake-word-models (Hey Home Assistant
and Okay Computer are its lightly trained "experiments"). Hey Luna and about 900 other
community words can still be added under Voice › More wake words; a clock that already has
Hey Luna keeps it.

## 0.108.1 (2026-11-17)

The vendor script wifi_display.sh no longer runs: it only served the stock clock app's Wi-Fi icon.

It ran `wpa_cli` every 2 seconds to keep a file for the stock clock app, which clockd
replaces. start.sh now stops it at boot (`touch /data/local/clockd/use-wifi_display` keeps
it); the script itself stays on the clock.

## 0.108.0 (2026-11-17)

Optional online update check: System › Update can ask GitHub for new versions (off until you switch it on).

Once a day (and on "Check now") the clock asks GitHub's API for the latest release; when a
newer one exists, "Download" fetches the package, which is then checked like an uploaded one
(signature, every file) and shown with what would change. Installing is always your click.
Off by default: with it off the clock never contacts the internet.

## 0.107.6 (2026-11-17)

Home Assistant no longer picks up the setup hotspot's address for the clock after a first install.

While the clock had joined the home network but the setup hotspot was still up, it announced
both addresses; Home Assistant could keep the hotspot's (192.168.255.249) and then never
reach the clock. The hotspot's address is now announced only while it is the clock's only one.

## 0.107.5 (2026-11-17)

The setup hotspot comes up about 30 s sooner after a first install, so the first try to join it works.

Without a saved network clockd no longer waits a fixed 20 s before opening the hotspot, and
start.sh no longer waits for DHCP before starting clockd. The installer's next steps say to
join once the face scrolls "setup": a computer may still list the hotspot from before the
restart.

## 0.107.4 (2026-11-17)

The installer downloads the latest release itself: `satellite/install-clock.sh --latest`.

Run without options on a PC that hasn't built the helpers, it recommends the latest
release and asks before downloading; `--build` builds from source without asking. The
package and its `SHA256SUMS` land in `satellite/build/downloads/` and get the same checks
as with `--release`.

## 0.107.3 (2026-11-17)

The installer can do a first install from a release package: no Go or build tools needed.

`satellite/install-clock.sh --release clockd-<version>-arm64.tar.gz` now also installs on a
clock without clockd, with every helper from the package (setup hotspot firewall, ssh,
WebRTC microphone). The PC checks each file against the package's manifest, the signature
when Go is installed, and `SHA256SUMS` when it sits next to the package. Without
`--release`, the installer says how to get adb and Go when they're missing.

## 0.107.2 (2026-11-17)

A teaser for the clock face: the time glitches apart and "luna" forms (Display › Clock face).

The segments start to flicker, symbols blink on and off and the noise grows until the time is
gone. The face goes dark for a moment, a lone segment sparking now and then, then the segments
settle slowly into "luna", which holds for a few seconds before the clock returns. About
12 s; visual only, no sound.

## 0.106.8 (2026-11-17)

First public release.

- Voice satellite over the ESPHome native API: on-device wake words (microWakeWord), a
  "Stop" word, timers and announcements from Home Assistant's Assist.
- Microphone: the raw mics through WebRTC audio processing and a learned two-mic echo null.
- Music Assistant speaker (Sendspin), MP3 and FLAC playback, a USB drive player.
- Clock face, status LEDs, night light and buttons driven directly; night schedule.
- Alarms kept on the clock, set from the web UI, the buttons or Home Assistant.
- Web UI for every setting, login by button, a first-time setup hotspot.
- Signed release packages, installed from System > Update with automatic rollback.
