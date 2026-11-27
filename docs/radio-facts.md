# RTL2832U and R820T: measured facts

Measurements of the RTL2832U and R820T on a real stick: register addresses, values, data
formats and what each one measurably does. They were made for a clean-room rewrite
([radio-provenance.md](radio-provenance.md), section 7). That rewrite was dropped when the
project moved to GPLv3 (2026-11-27). The facts remain a reference for debugging the
driver and for adding tuners.

**Where the register addresses come from:** the addresses come from the derived setup
this page replaces (from the model's memory of librtlsdr). That is unavoidable: the
RTL2832U has no public datasheet. Each one is listed here only because a measurement
below shows what it does. Addresses and values are interface facts. Nothing on this page
is anyone's code.

## Setup

- **Hardware:** clock #2 (eng firmware). Stick: generic RTL2838UHIDIR, USB 0bda:2838,
  RTL2832U + R820T/R820T2, 28.8 MHz crystal, the small whip antenna, indoors in an
  office.
- **Date:** 2026-11-25.
- **Station:** SUOMIPOP 106.3 MHz, the strongest local one (stereo pilot about 53 dB in
  the clock's search). Captures were tuned to 106.55 MHz, so the station should sit at
  **−250 kHz** in the baseband. A mirrored spectrum then shows up at +250 kHz.
- **Capture:** 1.92 MS/s u8 I/Q, 0.5 s per capture, tuner gain codes LNA 10, mixer 8,
  VGA 8.
- **Metrics** (`satellite/tools/rtlfm/facts_analyze.py`):
  - DC and I/Q balance
  - level in dBFS
  - noise floor at 0.6–0.8 MHz
  - station band power at −250 kHz and at the mirror +250 kHz, both over noise
  - the 19 kHz stereo pilot after FM demodulation, over the 17–18 kHz band
  The pilot varies with the programme: 8–18 dB across runs with nothing changed.
- **Tools:** `rtlfm facts-*` (`satellite/tools/rtlfm/facts.go`). The raw captures are
  kept in the private repo's git-ignored `test/radio-facts/`.

## A. Access to the chip (confirmed working on this stick)

All access goes through USB vendor control transfers (request 0); samples come in over
bulk endpoint 0x81.

| Target | Read | Write |
|---|---|---|
| Block register (USB block 1, system block 2) | type 0xC0, wValue = address, wIndex = block << 8 | type 0x40, wValue = address, wIndex = block << 8 \| 0x10 |
| Demodulator register (page 0–4) | type 0xC0, wValue = address << 8 \| 0x20, wIndex = page | type 0x40, wValue = address << 8 \| 0x20, wIndex = 0x10 \| page |
| Tuner over I2C (block 6) | type 0xC0, wValue = I2C address, wIndex = 6 << 8 | type 0x40, wValue = I2C address, wIndex = 6 << 8 \| 0x10 |

Two-byte registers are big-endian.

**Measured:**
- **Message size:** at most **16 bytes** per I2C transfer in either direction. That is 16
  registers per read (R0–R15), or the register number plus **15 registers** per write.
  17 or more bytes fail with EPIPE, and the tuner still answers afterwards.
- **I2C repeater:** demod 1:0x01 = 0x18 lets I2C through to the tuner, 0x10 closes it.
  With it closed, a tuner read fails (EPIPE); with it open, R0 reads 0x69 (0x96
  bit-reversed, as the R820T2 document says).
- **Powered-down demodulator:** every demod register read fails (EPIPE) until block
  2:0x3000 is written (see B).
- **No extra read after a demodulator write:** a read after each demod write is not
  needed. All registers were first set back to their power-up values; then the whole
  setup ran without the read, every register read back as written, and the radio worked
  (pilot 16.6 dB). This was tested once, on this host controller.
- **USB re-enumeration** (host controller rebind, VBUS stays on) resets block 1:0x2000 to
  0x01 and 1:0x2148 to 0x0000. The demodulator keeps its registers.
- **Power-up:** the values below were read after a real power cycle (the clock's mains
  socket off for 15 s, then the USB port switched to host mode). The demodulator was
  read after only the two power writes (2:0x300b, 2:0x3000).

## B. The setup writes: power-up value, our value, measured effect

Method: each capture ran the whole setup with one write changed back to its power-up
value. Three runs each; the baseline is 3 runs with nothing changed.

- **Baseline:** level −12.5..−11.6 dBFS, noise −30.9..−30.1 dB, station +4.6..+5.9 dB,
  mirror −5.3..−4.9 dB, pilot 11.4..16.7 dB, DC under 0.2 LSB, I/Q balance within
  0.02 dB, I/Q correlation 0.000.

| Register | Power-up | Ours | Effect of the power-up value (3 runs) | Verdict |
|---|---|---|---|---|
| block 2:0x3000 (1 byte) | 0x20 | 0xe8 | demodulator does not answer (EPIPE) | **required**: powers the demodulator. 0x20 is also our "off" value at close |
| block 2:0x300b (1) | 0x02 | 0x22 | none measurable | not needed on this stick (only the I input is used, see 0:0x08) |
| block 1:0x2000 (1) | 0x01 | 0x09 | none measurable | not needed on this stick |
| block 1:0x2158 (2) | 0x0002 | 0x0002 | already equal | not needed |
| block 1:0x2148 (2) | 0x0000 | 0x1002 | none (streaming writes it anyway) | the stream start writes 0x1002 then 0x0000 (FIFO stall and clear). Whether the stall is needed is unmeasured |
| demod 0:0x19 (1) | 0x20 | 0x05 | **no samples at all** (bulk read times out) | **required** |
| demod 0:0x06 (1) | 0x00 | 0x80 | **no samples at all** | **required** |
| demod 1:0x16–0x17 (2) | 0x3fce | 0x0000 | station moved, no stereo pilot, peak at −163 kHz | **required**: an extra frequency shift, must be 0 |
| demod 1:0x15 (1), final value | 0x06 | 0x01 | **spectrum mirrored**: station at +196 kHz, mirror +12 dB, no pilot | **required**: bit 0 = spectrum inversion, needed with this tuner (LO above the station) |
| demod 1:0x15 (1), earlier write of 0x00 | 0x06 | 0x00 | none (overwritten by the final 0x01) | not needed |
| demod 1:0x93 (1) | 0x0f | 0xf0 | signal and noise both −1.7 dB, same SNR and pilot | optional: digital gain only |
| demod 1:0xb1 (1) | 0x1f | 0x1a | signal and noise both −1.7 dB, same SNR and pilot | optional: digital gain only; DC stayed under 0.2 LSB either way |
| demod 1:0x01, 0x14 then 0x10 (reset pulse) | 0x10 | pulse | one run of three −1.9 dB, two none | inconclusive |
| demod 1:0x94 (1) | 0xff | 0x0f | none measurable | not needed on this stick |
| demod 1:0x11 (1) | 0x07 | 0x00 | none measurable | not needed on this stick |
| demod 1:0x04 (1) | 0xc0 | 0x00 | none measurable | not needed on this stick |
| demod 0:0x61 (1) | 0xe0 | 0x60 | none measurable | not needed on this stick |
| demod 0:0x08 (1) | 0x0d | 0x4d | none measurable | not needed on this stick |

"None measurable" means no change in level, noise, DC, I/Q balance, station, mirror or
pilot in 0.5 s captures on one station. A write may still matter over hours, with
temperature, on other sticks, or for things this test does not see. Keeping one of
these is a design decision for the rewrite, and it must give its own reason.

## C. Data formats (measured)

- **IF register, demod 1:0x19–0x1b:** 22 bits, 0x19 holding the top 6 bits. The value
  is −IF × 2²² / 28.8 MHz in two's complement. The station moves in the baseband by
  exactly the change in IF, same sign: +100 kHz moved the station by +99 and +97 kHz.
  Power-up value 0x35d75d.
- **Rate register, demod 1:0x9f–0xa2:** written as two 2-byte registers at 0x9f and
  0xa1. It holds 28.8 MHz × 2²² / rate, with the low 2 bits cleared, in 28 bits. The
  chip **repeats bit 27 as bit 28**.
  - At 240 kS/s the register value is 0x0E000000. The stick delivers 237–240 kS/s,
    which is right only with the repeated bit; without it the value would mean
    514 kS/s.
  - Measured delivered rates: 240 k, 1.0 M, 1.92 M, 2.4 M and 3.2 MS/s, all within
    0.3–0.6 % of the request (the timing window).
  - Valid ranges (rates outside them were not tried): 225–300 kS/s and 0.9–3.2 MS/s.
- **Channel FIR, demod 1:0x1c–0x2f (20 bytes), power-up values `09 f6 d2 a7 9a c9 27 77
  06 bf ec f4 4f 0b fc 01 63 35 54 a7`:** a symmetric 32-tap filter at 28.8 MHz, given
  as its 16 taps from the outermost inwards.
  - Taps 0–7 are signed 8-bit, one byte each.
  - Taps 8–15 are signed 12-bit, two taps packed in three bytes: the first tap's high
    8 bits, then its low 4 bits with the second tap's high 4 bits, then the second
    tap's low 8 bits.
  - The taps sum to about 4096 for unity gain (32 taps, symmetric).
  - **Measured:** the registers read back as written. Noise PSD ratios between filters
    (300 kHz and 800 kHz designs against 3 MHz) match the response predicted from the
    taps within 0.5 dB up to 700 kHz. Nearer the band edge, noise folds in from beyond
    ±0.96 MHz, as expected. A wrong layout would give a different shape.
  - The filter matters against stations outside ±0.96 MHz folding in (the ghost
    stations: Engineering Notes 2026-11-21).

## D. R820T (step 4): the three values the Rafael document does not give

- **PLL lock range:** the lock bit (R2 bit 6) was swept with mixer divider code 0. It
  locks **from 1716 MHz to 3740 MHz** (no lock at 1714 MHz or at 3745 MHz). One stick, at
  room temperature, lock bit only; phase noise near the ends was not measured. A divider
  step doubles the frequency, so a band of 2:1 inside that range covers every
  frequency. The old limits 1770–3540 MHz were inherited, not measured.
- **I2C writes:** up to 15 registers per message (A). The old 7 per message was
  inherited and is merely safe.
- **IF:** the tuner and demodulator IF together were swept over 2.0, 2.5, 3.0, 3.57,
  4.0, 4.5, 5.0 and 6.0 MHz.
  - Station SNR (+5.5..+6.1 dB), mirror (−5.4..−3.7 dB) and pilot (10–14 dB) are the
    same at all of them.
  - At 4.5 and 6.0 MHz the level is 1.5–2 dB lower (the tuner's IF filter edge, at its
    widest setting).
  - So any IF from 2 to 4 MHz works equally; 3.57 MHz is not special. The rewrite picks
    its own value in that range and states why.

## E. Limits

- One stick, one station, one room, short captures. Other sticks (no TCXO, the R820T
  without the "2") can differ.
- "Required" is solid: the radio breaks without the write. "Not needed" only means not
  measurable here.
- After the rewrite, re-check it with a longer run: half an hour of streaming, the
  station search, and the ghost-station check from v1.6.8.

## F. Rewrite check (2026-11-26)

Same stick, station, gain and 0.5 s captures as above; the rewritten setup uses an IF
of 3.0 MHz, 15-register tuner writes and the bridge's full 16-byte shadow read (R0–R15).

- **Rate write order (measured with `rtlfm facts-rate`):** 0x9f takes the high word and
  0xa1 the low word. A final write of the high word to 0x9f, after the channel FIR load,
  is needed for the requested ratio. Without that last write the two rates above came
  out swapped. With it, 240 kS/s delivered 238,779 S/s over 20 s and 1.92 MS/s delivered
  1,916,293 S/s over 10 s (0.5 % and 0.2 % low).
- **Setup ablation with the rewritten code:** the five required writes in B break the
  same way when returned to their power-up values (no demodulator answer, no samples,
  moved spectrum, or a mirrored spectrum). The two attenuation writes again had no
  measurable effect on station or pilot.
- **FIR:** the 800 kHz filter's 20 registers read back exactly as packed. The unit test
  covers the round trip, including negative 12-bit taps.
- **IF comparison rule:** in one session, capture three times at 3.0 MHz and three times
  at 3.57 MHz as the control, alternating. Accept 3.0 MHz if station-to-mirror and
  station-to-noise are both within about 1 dB of the control. Result (station and mirror
  are dB over the noise floor): station-to-mirror was 9.1–9.3 dB at 3.0 MHz and 10.1–10.3
  dB at 3.57 MHz (difference 1.0 dB); station-to-noise was 6.5–6.6 dB and 6.7–7.0 dB
  (difference 0.4 dB). 3.0 MHz is accepted.
- **Station-search pilot check:** against the pre-rewrite list, SUOMIPOP measured 31.0 dB
  in one search and was below the 25 dB threshold in the next (previously 53.0 dB); the
  other four checked stations stayed within the usual programme spread. A direct
  off-centre baseline was healthy (level −12.0 dBFS, station +6.8 dB, pilot 13.5 dB). The
  search's own 100 ms metric at 106.3 MHz ranged 4.6–48.5 dB over 293 windows (median
  24.7 dB; 152 below 25 dB), so one search can miss the station depending on the programme
  and the window. This is not a loss of the station in I/Q.
