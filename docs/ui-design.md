# Web UI design: Luna

Rules for the clockd web UI (`satellite/internal/web/ui/index.html`, `setup.html`).
Decided 2026-11-07 with the user, after comparing Material, Radix and our own skin in
the gallery: `satellite/tools/uigallery/gallery.html` (standalone, not embedded; open it
in a browser or serve it with `python3 -m http.server` from that directory). The gallery
is the reference implementation: when this file and the gallery disagree, fix one of them.

## Where it lives

The setup-hotspot page (setup.html) has no style choice: it always uses the Luna dark look.

Luna is a per-browser style (Appearance > Style: Classic / Luna, localStorage
`clockd-style`, `body[data-style=luna]`). Classic is the original look and stays the
default. The Luna CSS is one block at the end of the `<style>` in index.html; light themes
are told apart with `html[data-scheme=light]` (set by applyTheme). Layout rules (rows, button
weights, help, status lines) apply to both styles; Luna only changes the look. Appearance >
Digits (Luna only): seven-segment digits on the dashboard only, or also for slider values.

## Principles

- **No external code.** No frameworks, component libraries, CDNs or web fonts. Everything is
  hand-written CSS/JS inside the single HTML file; nothing to build. Material and Radix were
  used as *guidelines only* (no code copied, nothing to credit).
- **Usage rules from Material 3, look of our own ("Luna").** Material decides *which*
  control is used for what; the look comes from the clock itself: white lights, seven-segment
  digits, the black face, the fabric cover.
- **One device element per control.** A switch may look like an LED, but it behaves exactly
  like a switch. Restraint over decoration.
- **The special look is for dark themes.** Black slots, white LEDs and black display windows
  only work on a dark page. Light themes get the same shapes and behaviour in theme colours
  (grey slots, accent for "lit", digits in the text colour, no glow).
- **All 16 themes keep working.** Every colour derives from the theme variables (`--bg`,
  `--card`, `--line`, `--fg`, `--dim`, `--acc`, `--accfg`, `--ok`, `--bad`, `--warn`,
  `--input`) with `color-mix()`. No hard-coded colours except the dark-mode device parts
  (white light, black wells/glass).

## Which control for what

| Need | Control | Notes |
|---|---|---|
| One on/off setting | **Switch row** (`label.set`): title + one-line description left, switch right | Replaces plain checkboxes. The whole row toggles. Never put a `<button>` inside the label (it becomes the label's control): the "?" is a `span.q`. |
| An on/off with a number that only matters while on | **Switch row + subfield** (`.subfield`): "Over [10] minutes" under the row, shown only while on | e.g. Fade in / out (0 = off). Switching on restores the last number. |
| Several options of one group | **Chips** (`<button aria-pressed>`) | e.g. clock face: Clock face / 24-hour / Blinking colon / Eyes. |
| Pick one of 2-5 | **Segmented control** | e.g. Wake word sensitivity, snooze length. More than 5: a select (alarm sound). |
| Pick one of many | **Select** | Weather entity, wake word model lists, themes stay a picker grid. |
| A number in a range | **Slider row** (`.srow`): name + value left, a shorter slider right, one line | Keeps the rhythm of the switch rows. Value as digits with Luna + Digits: Everywhere. |
| A colour per item of a list | **One editor for the selected item** under the list (swatches: presets, one custom picker, Off); the rows show only the current choice | e.g. Status LEDs. Not a dropdown + colour box in every row. Keep the presets few (Classic, Classic dim, White, White dim). |
| Read-only status | **Badge** (LED + label) | Never looks clickable: no border, no hover, small. |
| Action | **Button**, weight by importance | Classes: `btn` = primary (filled, one per card: the action the card exists for), `btn sec` = ordinary actions, `btn quiet` = backing out (Cancel, Hide, Deny), `btn danger` = can't be undone / interrupts the clock (Restart clockd, Log out, Remove token or password). Per-row Remove/Forget in a list: `btn quiet del` (grey, red on hover) so a list isn't a wall of red; icon-only trash buttons for compact rows. |
| Long explanations | **"?" help** next to the title or at the end of a one-line hint, expands in place (`.set-help`) | Descriptions stay one line. Safety warnings (e.g. plain-http password) stay fully visible. |
| Whether an optional feature is in use | **Status line at the top of its card** (`.ha-state`): LED + "In use · connected" / "In use · not reachable" / "Not in use" + one detail line | Checked live where possible (e.g. the HA API lists entities). Empty fields must never be the only sign of state. |
| A stored secret (token, password) | Field stays empty; a status line above it says so in words ("Token stored", "· works" once tested), and the empty field is hidden behind a "Replace" button on that line (an empty box invites typing); while replacing: Cancel (quiet) and Remove (quiet del, right) next to the primary button. The primary "Save & test" only shows when something changed; Set becomes Replace; Remove disabled without one. In a step card that ends in one "Save & test" (Home Assistant API), that button stores it: no Set, and "Remove token" sits in the actions row after it, shown only while one is stored | Secrets are never sent back to the browser. |
| A card the user sets up once (steps: address, token, …) | Numbered `.subhead` steps. A finished step collapses to a status line (LED + "… set · works" + the value) with ONE button at its right (Change / Replace); its fields, how-to hints and switches show only while changing. One primary "Save & test" that appears only when something changed, one Cancel (quiet) that restores the saved state. Offer found values (discovery) as selectable rows above the field | Home Assistant API card (v0.87.5): the before/after the user approved. |
| Buttons next to a field | Only a button that acts on THAT field's value (Show, Copy). Searching, removing or saving goes elsewhere: a search button next to its results, Remove in the card's actions row after the primary button | The first thing after typing into a field must not be Remove or an unrelated action. |

## Scales (shared)

- Radius: 4 / 6 / 10 / 14 px / full (`--r-xs` ... `--r-l`, `--r-full`). Luna cards and tiles
  use 14, inputs/buttons/chips/segmented 10.
- Type: 12 / 13 / 15 / 17 / 22 px. Body 15. Card titles 17 semibold, descriptions 13 `--dim`,
  section labels 12 uppercase `--dim`.
- Spacing: 4 / 8 / 12 / 16 / 24 px. Card padding 16. Settings rows at least 56 px tall.
- Touch target: 40 px minimum.
- One "selected" idiom per control family (no new ones): switches and chips light up,
  segmented carries a light strip, list rows (tunes, LEDs) get the tinted border, swatches
  get a ring.

## Page rules

- **Show a control only while the chosen options use it.** A slider or field that has no
  effect with the current choice is hidden (e.g. Night schedule: Dimmed brightness only for
  "Dimmed", Night light level unless "Leave alone", the dark level only for the "...when
  dark" options). Fewer controls on screen beats explaining why one does nothing.
- **Long cards get small group headings** (`.subhead`: quiet caps + a thin line, no extra
  boxes), e.g. Night schedule: When / Clock face / Night light / Room / Override. A heading
  hides with its rows when none of them apply.
- **Side-list groups stay short** (a tab's `data-group`s): about four cards per group at
  most. A new card joins the group it belongs to by meaning, or starts a new one; when a
  group passes four, regroup the tab rather than appending (System, v0.106.3: one "Device"
  group of nine became Device / Network & access / Maintenance / Diagnostics). Card titles
  stay as they are: they are the page slugs (`#system/update`).
- **Rhythm:** a card reads as an even list of one-line rows (switch rows, slider rows,
  segmented with its label). Anything taller (previews, lists, editors) is a deliberate block.
- **No repeated information** on one screen (the version is in the footer, the clock picker
  and System > Device; the dashboard tiles don't repeat the hero or the gauges).
- **Short segmented labels** (`data-short`) need the full wording: it shows under the
  buttons for the chosen option (segHint). Check 4-5 option groups at phone width.
- **SVG ids must be unique per picture** when a picture is drawn more than once (face
  previews): shared gradient/filter ids make one copy borrow another's, which breaks when
  that one is on a hidden tab.

## Luna look

Dark themes:

- **Lights are white with a soft glow** (`--lit: #fff`, glow `0 0 5px rgba(255,255,255,.75),
  0 0 12px rgba(255,255,255,.35)`). The clock face only has white lights: no warm or coloured
  "on" states.
  - Switch: recessed dark slot (`--well` ~#0e0f12, inset shadow), a dark dot that turns into a
    white glowing LED when on.
  - Chip: outlined key with a small LED dot that lights when selected.
  - Segmented: dark slot; the chosen segment carries a short glowing white strip.
- **Digits are seven-segment, white, upright** (no italic skew), unlit segments faintly
  visible (6 %), always inside a small black display window (`.lcd`, ~#0b0c0e).
- **Controls you operate use the theme accent with a soft accent glow:** slider fill and knob
  (thin 4 px line, no outline), primary button halo (stronger on hover), focused inputs,
  tile hover. Saving state: the primary button pulses slowly (the clock's listening pulse).
- **Glow is optional** (Appearance > Glow, default off on touch screens, on with a mouse): every blur (SVG
  filters, drop-shadows) must sit under a selector that `body[data-glow=off]` can switch off.
  Blurs are what make a zoomed-in picture heavy on phones.
- **The header clock** (every page, both styles, all themes): the time as seven-segment
  digits in a small black window; it links to the dashboard. The dashboard hero shows the
  live face picture instead of a second time.
- **Fabric** only where something *is* the clock (device previews, the dashboard hero); plain
  cards everywhere else.
- **Depth:** cards and tiles are raised (top-edge highlight + soft shadow; Glow off keeps only
  the highlight). The page has a faint accent "moonlight" glow from the top centre.
- **Moon** (Appearance > Moon, per browser, dark themes only): a soft round / crescent /
  today's-phase moon behind the page, default "At night" (shown while the clock's night
  schedule or night mode is on), top right, pale warm, its glow replacing the top-centre
  moonlight. Decoration in the background, so the warm tint is allowed here; no blur filters.

Light themes: `--lit` = accent, no glow, slots `color-mix(--fg 7%, --card)`, digits in
`--fg` without a window, switch track tinted with the accent when on.

## Status: LED style

Status is shown like the clock's own lights, not as traffic lights:

| Meaning | Look |
|---|---|
| Fine / connected / in use | lit white dot (light themes: accent dot) |
| Off / neutral / saved / experimental | hollow ring |
| Needs a look (near miss, degraded) | white dot, slow blink |
| Error (not connected, failed) | red dot (`--bad`), emergency flash: a fast double flash with an expanding halo every 1.2 s. The only colour, so it stands out |

`prefers-reduced-motion` turns every blink and flash into a steady light.

The label carries the specific meaning ("connected", "in use"); the dot only says fine /
neutral / attention / error. Keep `--bad` clearly apart from each theme's accent
(Sepia's red was moved to crimson #b8213f for this).

## Dashboard (in use since v0.68.0)

- Service tiles: no tinted icon squares; the icon sits in a dark well (light themes: grey),
  one status LED right of the title. An error tile also gets a faint red border and its
  subtitle in red.
- Metric rings become a **dial of 20 LED ticks**: lit ticks = the value (white; light
  themes: accent). High value: the head tick blinks. Error level: the lit ticks turn red.
  The number next to it in seven-segment digits in a black window, unit in small text.

## Not decided yet

- Phone layout details beyond the existing accordion (the gallery's Phone view is 390 px).
- Night schedule: small group headings (When / Clock face / Night light / Room) if the page
  still reads heavy.
