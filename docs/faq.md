# FAQ

Short answers; the details are in the [README](../README.md) and [DETAILS.md](../DETAILS.md).

## The device

**Which clock is this for?**
- Only the **Lenovo Smart Clock Essential** (CD-4N341Y): the one with a segment display
  and a night light.
- Not the Lenovo Smart Clock (1st gen, touchscreen), the Smart Clock 2 or the Smart
  Displays: different hardware, and they won't work.

**Is it Android? Can I install APKs?**
- No. The stock firmware is Google's Cast platform. It borrows Android's low-level parts
  (`init`, `adb`, `getprop`, `logcat`), which is why it looks like Android over adb.
- It has none of the app layer: no Android framework, no app runtime, no package manager,
  no Play services. Nothing on the clock can run an APK.
- The hardware couldn't run Android apps anyway: 480 MB RAM with no swap, about 100 MB free
  storage, and a segment display (digits and a few icons), not a screen. No touch.
- clockd doesn't add Android. It replaces the Cast software with one Go program.
- adb is just the cable. There's no Android on the other end.

**Then what can I install?**
- **clockd updates:** signed packages, from the web UI (System › Update), from Home
  Assistant, or with `install-clock.sh`.
- **Wake words:** web UI › Voice › More wake words (the ESPHome set and about 900
  community phrases).
- **Your own programs:** static Linux binaries for arm64 (Go, or C built against musl),
  copied over ssh or adb into `/data/local/`. Don't rely on the clock's own libraries.
- To start them at boot: `/data/local/clockd/user/boot.sh` (see "Your own programs" in
  DETAILS). Updates leave `user/` alone; don't edit clockd's own files, an update replaces them.
- Keep them light: the microphone needs the CPU and memory headroom.
- Usually "I want to install X" really means "I want the clock to do X". That's mostly
  a job for Home Assistant: automations, announcements, text on the face, alarms.

**Can I get a full Linux (Debian, postmarketOS) on it?**
- Not with this project. clockd runs on top of the stock firmware and doesn't change the
  boot chain.

## Getting it running

**Does clockd root or unlock the clock?**
- No. Unlocking is a separate step with **[luna-unlock](https://github.com/pullasuti/luna-unlock)**. clockd installs on a clock that's
  already unlocked. Unlock questions belong there.

**What do I need?**
- A Linux or macOS PC with adb and curl, a USB-A to USB-A cable (male on both ends; the
  clock's socket is USB-A), and Home Assistant for voice.
- Windows isn't covered by the installer.

**Should I back up first?**
- Yes: `satellite/tools/dump-partitions.sh` (read-only, about 2-3 minutes over USB). Keep
  the dump private: it holds your serial number, MAC addresses and Wi-Fi settings.

**Is it permanent? Can I go back?**
- Nothing in the firmware is deleted. Hold **alarm + Vol-** for 12 s and the clock restarts
  into the stock software; hold them again there to factory reset it.
- Stock means Google again: it connects to Google straight away.

## Using it

**Does it still have Google Assistant?**
- No. You talk to *your* Home Assistant (Assist). The wake word is detected on the clock;
  the speech goes to HA, and HA's own pipeline settings decide whether speech recognition
  is local or cloud.

**Does it work without Home Assistant?**
- The clock and alarms do, once the time is known. Voice and weather need Home Assistant;
  the time comes from Home Assistant or an NTP server you name.

**Can I cast to it, or use Spotify Connect / AirPlay?**
- No Cast, Spotify Connect or AirPlay. Music comes in through:
  - **Music Assistant** (Sendspin, synchronised with other speakers), which can play
    Spotify and other services;
  - **DLNA** apps (switch it on in the web UI);
  - a **USB drive** with music on it.

**Is it a Bluetooth speaker?**
- Not yet. It can forward nearby Bluetooth Low Energy sensors to Home Assistant (a
  Bluetooth proxy, off by default).

**Which wake words?**
- "Okay Nabu" by default; more in web UI › Voice › More wake words. Up to two at a time,
  plus "Stop" while it plays.
- The wake word is separate from the language you speak to Assist.

**Can I set the clock's alarms by voice?**
- Yes, through a Home Assistant script. The quickest way is the blueprint
  [docs/blueprints/clockd_alarms.yaml](blueprints/clockd_alarms.yaml): import it
  (Settings › Automations & scenes › Blueprints › Import, with the file's GitHub URL),
  create one script from it, pick your clocks from the list and expose the script to
  Assist. It sets, lists, deletes and switches alarms: "wake me up at 6:45 on weekdays",
  "what alarms do I have?", "turn off the work alarm in the bedroom". The first clock is
  the default; a clock is picked by its area or name, and an unknown clock gets an error
  listing them, so the model can retry. Prompt lines that help a small model (use your
  script's entity id as the tool name):

  ```
  - Alarms on the clocks: always use the clockd_alarms_on_the_clocks tool. Never
    pretend: if the tool returns an error, say what failed; only say it is done when
    the tool succeeded.
    - New alarm: action=set, time=HH:MM in 24-hour time (quarter to seven = 06:45,
      7:30 pm = 19:30), days=once|daily|weekdays|weekends|mon,wed,fri, label optional.
    - Which alarms: action=list (leave clock empty for all clocks).
    - Delete or switch: action=delete|on|off, alarm=the label, or the HH:MM if it has
      no label. If unsure which alarm, call list first. One call per alarm.
    - clock: the room or name the user said (e.g. Makuuhuone); empty for the default.
      If the tool says no clock matches, pick one from the list it gives and retry.
    - One tool call per message: wait for each result before the next call (some local
      model servers garble a second call in the same message).
  ```
- By hand: the clock gives HA four actions:
  `esphome.<clock>_set_alarm`, `_switch_alarm`, `_delete_alarm` and `_list_alarms`
  (`<clock>` is the clock's name with `-` as `_`). Wrap one in a script and expose the
  script to Assist; a language model (LLM) conversation agent then sees it as a tool, with
  the field descriptions as its parameters:

  ```yaml
  alias: Set clock alarm
  description: >-
    Sets an alarm on the bedroom clock. Use for "wake me up at ...", "set an alarm for ...".
  fields:
    time:
      description: Alarm time, 24-hour HH:MM, e.g. 06:45
      required: true
      selector: { text: }
    days:
      description: once (default), daily, weekdays, weekends, or day names like mon,wed,fri
      selector: { text: }
    label:
      description: Optional name for the alarm, e.g. work
      selector: { text: }
  sequence:
    - action: esphome.lenovo_clock_xxxx_set_alarm
      data:
        time: "{{ time }}"
        days: "{{ days | default('once') }}"
        label: "{{ label | default('') }}"
  mode: single
  ```
- Expose it: the script's ⋮ › Settings › Voice assistants. Tell the model to always use
  it, as with timers (below).
- The other actions work the same way: an alarm is named by its label, or by its HH:MM when
  it has none. `_list_alarms` returns the alarms and the next one, for "what alarms do I
  have?" (a script with a response variable).
- Without a language model, a custom sentence automation ("wake me up at {time}") can
  call the same action.

**Where are the settings?**
- In the clock's web UI at `http://<clock-ip>/` (or its name on your network). New
  browsers are approved with a button press on the clock: a code shows on the face.

## Privacy and security

**Does it talk to the internet?**
- clockd doesn't. Everything goes to your own network: Home Assistant, Music Assistant,
  your router.
- The one optional exception: the update check (off by default) asks GitHub once a day.
- In the first seconds after boot a vendor daemon can still reach Google before clockd
  stops it. For a hard guarantee, block the clock's internet access on your router; it
  needs none.

**Is adb a security hole?**
- While the clock is unlocked, adb gives root over USB and Wi-Fi (port 5555) without a
  password. Set up an ssh key in the web UI, then lock the clock again
  (`luna-unlock.py --lock`). Everything keeps working; updates go through the web UI.
- Engineering-firmware clocks: before clockd is installed, the vendor's setup hotspot runs
  an ssh server that accepts root with any password. Install clockd soon after unlocking.

**Is the web UI encrypted?**
- No, it's plain HTTP on your network. The Home Assistant connection is encrypted.

## Problems

**Something doesn't work. What should I include in a report?**
- The firmware build: `adb shell getprop ro.build.display.id`
- The clockd version (web UI › System, or the Firmware entity in HA)
- The log: web UI › System › Logs, around the time it happened
- Check the log for Wi-Fi names, IP addresses or tokens before posting it.

**The face shows "Err" and nothing else works.**
- clockd failed to start five times in a row, so the clock is in rescue mode.
- Power-cycle to try again. If it keeps coming back, hold **alarm + Vol-** for 12 s (no
  sound in this mode) to go back to the stock software, and report it with the log
  (`/tmp/clockd.log`, over ssh or adb, before the power cycle).
- A user boot script that ran on that boot is switched off (`user/disabled`).

**The assistant says a timer is set, but it never rings.**
- Then no timer was started: Home Assistant's language model (LLM) conversation agent answered without
  using its timer tool. Plain phrases ("set a timer for five minutes") work when the
  voice assistant's "Prefer handling commands locally" is on; anything else goes to the language model.
- Let the model control Home Assistant (the agent's "Control Home Assistant" / Assist
  option) and tell it in its instructions to always use the tools, for example:

  ```
  - Timers: never pretend. To set, check, change or cancel a timer, always call the
    matching tool (HassStartTimer, HassTimerStatus, HassCancelTimer, HassIncreaseTimer,
    HassDecreaseTimer, HassPauseTimer, HassUnpauseTimer). Only say a timer is set after
    the tool call succeeded.
  ```
- Start a new conversation to test: in an ongoing one the model tends to copy its own
  earlier answers. Settings › Voice assistants › ⋮ › Debug shows whether a tool was called.
- The model must support tool calling. A timer that really started shows on the clock's
  log as `timer event=STARTED`.

**The clock went back to the stock software by itself.**
- An update that keeps crashing rolls back to the previous clockd on its own; that
  doesn't bring back stock. Stock comes back with the 12 s button hold or after a factory
  reset. Run `install-clock.sh` again to return.
