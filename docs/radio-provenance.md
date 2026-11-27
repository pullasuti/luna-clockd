# FM radio code: provenance audit

Where the FM radio code in luna-clockd came from and what it was compared against. This
is the record to rely on if the origin of the code is ever questioned. **The rules for
adding radio code today are in section 8.** Sections 1–7, 9 and 10 are the history of an
audit made while the project was still Apache-2.0.

> **Decision 2026-11-27: the project moved to the GNU GPL v3 or later (from 1.7.0).**
> The clean-room rewrite (section 10) passed the similarity check but broke the sample
> rate after tuning, and keeping LLM-written code free of GPL influence proved
> unreliable. So clockd 1.6.30 went back to the original driver and credits librtlsdr
> (GPL-2.0-or-later, which may be used under GPLv3). The audit below stays as the record
> of where the code came from. What still matters: GPL-2.0-only code (most Linux kernel
> drivers) cannot go into GPLv3, and borrowed code is credited.
>
> What this replaced: the Apache-2.0 standard of section 1, the plan of section 7 and the
> clean-room rewrite of section 10 no longer apply. The measurements in
> [radio-facts.md](radio-facts.md) stay useful as a description of the chip.

**Audit:** 2026-11-25, clockd 1.6.28 (private tree; the radio has not been published yet).
**Result:** the tuner, DSP, RDS and USB code are ours. The RTL2832U setup
(`internal/rtlsdr/rtl2832.go`) is **derived from librtlsdr** in order and wording and has
to be rewritten before release. A 140-line probe tool that is already public in 1.0.0
(`tools/rtlprobe`) has the same problem. Details and the plan below.

**2026-11-26 (clockd 1.6.29):** the derived setup was rewritten clean-room from measured
facts and passed the similarity re-check (section 10). Superseded the next day by the
licence change above: the rewrite was dropped and the original, credited code restored.

## 1. The standard applied

The radio code was written by a language model (LLM) working in this project, not by
hand. A language model can reproduce code it has seen in training without saying so, so
"nobody copied anything" is not enough. Our rule:

- **Allowed:** hardware facts. Register addresses, register values, bit layouts, formulas
  that the chip dictates, USB IDs, standards (IEC 62106 for RDS, the FM broadcast
  parameters). These are interface facts, needed for the hardware to work at all.
- **Not allowed:** the *expression* of someone else's code. That includes its structure,
  the order and grouping of steps when that order is a choice, tables in the same layout,
  comments, function shapes, and translation from C to Go.

The sources that matter are GPL-2.0-or-later: librtlsdr (Osmocom `rtl-sdr`) and the Linux
kernel's `rtl28xxu`, `rtl2832`, `rtl2832_sdr` and `r820t` drivers. Anything derived from
them cannot go into an Apache-2.0 project.

This standard is stricter than copyright law, which protects expression and not facts
(the audit is not legal advice). We use it anyway, so that the question never needs a
lawyer.

## 2. Scope

| File | Lines | First commit | Content |
|---|---|---|---|
| `satellite/internal/rtlsdr/usbfs.go` | 125 | 2026-11-21 (v1.2.0) | usbfs ioctls, stick discovery |
| `satellite/internal/rtlsdr/rtl2832.go` | 374 | 2026-11-21 (v1.2.0), FIR 2026-11-21 (v1.6.8) | RTL2832U setup, sample rate, IF, channel FIR, streaming |
| `satellite/internal/rtlsdr/r820t.go` | 224 | 2026-11-21 (v1.2.0) | R820T/R820T2 tuner |
| `satellite/internal/fmradio/demod.go` | 320 | v1.2.0, stereo v1.3.0, v1.6.8, v1.6.23 | FM discriminator, stereo PLL, de-emphasis |
| `satellite/internal/fmradio/decimate.go` | 160 | v1.6.8 | half-band decimation 1.92 MS/s → 240 kS/s |
| `satellite/internal/fmradio/rds.go` | 186 | v1.3.0 | RDS station names |
| `satellite/internal/web/resample.go` | 96 | v1.6.x | 48 → 32 kHz for the stream |
| `satellite/cmd/clockd/radio.go`, `radioha.go` | 810 | v1.2.0 onwards | clockd integration, search, favourites, HA |
| `satellite/tools/rtlfm` | 226 | v1.2.0 | test tool |
| `satellite/tools/rtlprobe` | 140 | 2026-11-13 | first probe (public in 1.0.0) |

## 3. How the code was written (process record)

From the git history and the development session logs, which are kept privately and were
searched for this audit:

- **2026-11-13, `tools/rtlprobe`:** written in one step as a feasibility probe. Its own
  comments say it follows "librtlsdr's register layout" and mark a block "librtlsdr
  init_baseband (start)". No source was downloaded or opened: the model wrote it **from
  memory** of librtlsdr. The tuner-ID table (I2C address, check register and value for
  R820T, R828D, E4000, FC0012/13, FC2580) is librtlsdr's probe list. The TODO plan of the
  same day even described the tuner work as a "translation of librtlsdr's tuner_r82xx",
  a plan that was later **not** followed (see r820t.go).
- **2026-11-20/21, v1.2.0, the driver:**
  - Before writing the tuner, the session downloaded Rafael Micro's *R820T2 Register
    Description* VER1.3 (published by Rafael in 2016, copy on rtl-sdr.com) and the R820T
    datasheet. It read the register description and rendered and transcribed its register
    matrix (Table 1-2).
  - `usbfs.go` and `rtl2832.go` were written first, in one step, again with no source
    open. The RTL2832U has no public datasheet, so this part, too, came from the model's
    memory of librtlsdr.
  - `r820t.go` was written from the register description: fields named and set as the
    document defines them, undocumented bits kept from the chip's power-up values. After
    a measurement on our stick showed that the RTL2832U's I2C bridge reads at most
    16 bytes, it was changed to write R16–R31 from the document's matrix.
  - No librtlsdr, kernel or other driver source was fetched in the session.
- **v1.3.0–v1.6.23, stereo, RDS, FIR, decimation:** written from signal-processing
  principles and the RDS standard, and tuned by measurements on recorded I/Q (Engineering
  Notes, 2026-11-21 and 2026-11-23). The channel FIR loader `SetFIR` (v1.6.8) was again
  written from memory of the chip's register format.
- **2026-11-24**, a separate session read the Linux 4.9 `rtl2832_sdr.c` (the
  kernel-driver question). That was after the driver had been written, and nothing from
  it went into the code.

## 4. Reference sources compared

| Source | Version | SHA-256 (first 16) |
|---|---|---|
| librtlsdr `src/librtlsdr.c` | osmocom/rtl-sdr master `797f8143266d` (2026-08-11) | `8c872716cd6959b3` |
| librtlsdr `src/tuner_r82xx.c` | same | `4d890206ebdb4dff` |
| librtlsdr `include/tuner_r82xx.h` | same | `f5b86dd012281418` |
| librtlsdr `src/rtl_fm.c` | same | `ea618424318dd352` |
| Linux `drivers/media/tuners/r820t.c` | 4.9.113 (the clock's own GPL release) | `a12491e2ff218cb6` |
| Linux `drivers/media/dvb-frontends/rtl2832.c` | 4.9.113 | `8ff775c947c564a9` |
| Linux `drivers/media/dvb-frontends/rtl2832_sdr.c` | 4.9.113 | `7d792b6ba90e029c` |
| Linux `drivers/media/usb/dvb-usb-v2/rtl28xxu.c` | 4.9.113 | `90ac0a319e8f57d4` |

These were downloaded for the audit only. They are not in the repository.

## 5. Method

1. **Reading side by side:** each of our functions against the reference functions that
   do the same job (baseband init, demodulator register write, sample rate, IF, FIR,
   tuner init/PLL/gain, FM demodulation in rtl_fm).
2. **Mechanical check, `satellite/tools/provcheck/provcheck.py`:** two measures for every
   pair of (our file, reference file).
   - **Code shape:** comments and strings dropped, identifiers and numbers replaced by
     placeholders, every run of 10 tokens hashed and matched. This finds copied or
     translated structure even after renaming.
   - **Constant order:** the numeric literals in order. Four or more identical constants
     in the same order are reported with line numbers on both sides; a copied init
     sequence or table shows up here.
3. **Calibration:** the same check on radio-free files of ours (`alarms.go`, `flac.go`,
   `mp3.go`) gives the noise level for Go against C: 0.3–0.7 % shared shape, no constant
   runs.

## 6. Findings

| File | Code shape | Constant runs | Verdict |
|---|---|---|---|
| `rtl2832.go` | 0.9 % (noise) | **14 + 8 in librtlsdr's order** (init), **7** (FIR packing) | **derived, rewrite** |
| `r820t.go` | 0.0 % | 3–5, all the document's gain/power bits | ours; 3 facts to re-measure |
| `usbfs.go` | 0.2 % | none | ours (ioctl numbers from the Linux UAPI header, which is meant for this use) |
| `demod.go`, `decimate.go` | 0.2–1.2 % (idioms: struct literals, comma lists) | none meaningful | ours |
| `rds.go` | 0.3 % | none meaningful | ours |
| `resample.go`, `radio.go`, `rtlfm` | ≤ 0.6 % | none | ours |
| `tools/rtlprobe` | 1.0–1.5 % | 4 | **derived (self-declared), remove** |

### rtl2832.go: derived

- **`initBaseband`:** the same 17 register writes as librtlsdr's `rtlsdr_init_baseband`,
  in the same order with the same values. Its comments paraphrase librtlsdr's ("soft
  reset", "no spectrum inversion yet / no adjacent channel rejection", "hold the DVB-T
  state machine", "no demod AGC", "no RF/IF AGC loop", "no PID filter"). Where it
  differs, it differs the way librtlsdr itself does for an R820T: it skips the DDC/IF
  clear loop and the FIR load, and adds librtlsdr's later R820T steps (zero-IF off, I
  input only, spectrum inversion: `0xb1 = 0x1a`, `0x08 = 0x4d`, `0x15 = 0x01`).
- **`demodWrite`:** the same shape as `rtlsdr_demod_write_reg`, including the dummy read
  of register 0x01 on page 0x0a after each write.
- **`SetFIR`:** the same steps as `rtlsdr_set_fir`: 8 taps checked against −128..127 and
  stored as bytes, then pairs checked against −2048..2047 and packed three bytes per
  pair, then written one register at a time from 0x1c.
- **`SetRate`, `setIF`:** the same formulas as `rtlsdr_set_sample_rate` and
  `rtlsdr_set_if_freq`, including the ratio mask `0x0ffffffc` and the repeated bit 27.
  The formulas are hardware facts. Their expression is short, but it came by the same
  route.
- **`Close`:** librtlsdr's deinit order (tuner exit through the repeater, then
  `DEMOD_CTL = 0x20`).

Nothing is a literal copy: it is Go and the code-shape score is at noise level. But by
the standard in section 1, the init sequence, the write helper and the FIR packer carry
librtlsdr's structure and comments, translated from the model's memory. That is
derivation.

**Our own parts of the file:** `LowpassFIR` (our own FIR design and its tuning against
ghost stations, v1.6.8), the streaming reader with its buffer pool, the 16-byte bridge
measurement, and the API.

### r820t.go: ours, with three borrowed facts

- **Its basis is the Rafael document.** It reads the chip back, sets named fields and
  writes R16–R31 from the published register matrix. Its init values are *not*
  librtlsdr's `r82xx_init_array` (for example R16 is 0x05 here and 0x6c there), and it
  has none of librtlsdr's structure: no frequency-range table, no calibration loop, no
  `r82xx_*` function shapes. The constant runs that the tool found are the document's
  LNA/mixer gain-mode and power bits (R5, R6 and R7), which every driver writes.
- **Three values are not in the Rafael document** and match librtlsdr. They are facts,
  but we should hold them from our own measurement, not from memory:
  - the IF of 3.57 MHz (`R82XX_IF_FREQ`)
  - the VCO range of 1770–3540 MHz (`vco_min`, `vco_max`). The document only gives
    the VCO indicator scale, 1.75–3.6 GHz.
  - 7 data bytes per I2C write (librtlsdr's `max_i2c_msg_len - 1`)

### DSP, RDS and integration: ours

- **The FM chain:** the discriminator, the pilot PLL, the stereo decoding with its blend,
  the de-emphasis, the half-band decimation and the 48 → 32 kHz resampler are standard
  signal processing. They were built and tuned here from our own measurements (Engineering
  Notes, 2026-11-21 and 2026-11-23), and their structure is not rtl_fm's.
- **RDS:** the decoder's method is its own. It integrates per pilot cycle and picks the
  bit slot by energy. rtl_fm has no RDS, and this is not the usual approach of the open
  decoders we know of (those use a separate bit-clock recovery). Its constants are the
  standard's: polynomial, offset words, and the EBU Latin character table.

### tools/rtlprobe: derived, and already public

The tool's own comments say it follows librtlsdr. Its init lines are the first seven of
`rtlsdr_init_baseband`, and its tuner-ID table is librtlsdr's probe list. It is in the
public luna **1.0.0** (2026-11-20). `tools/rtlfm` has replaced it, and nothing uses it
anymore.

## 7. What had to happen before the radio was published (superseded 2026-11-27)

| # | Work | Status |
|---|---|---|
| 1 | Remove `tools/rtlprobe` from the tree, so the next public export drops it. Note the removal and the reason in the release notes. | **done 2026-11-25** in the private tree; the public side waits for the 1.7.0 export (the user's go-ahead) |
| 2 | **Fact list, from measurement.** For every RTL2832U register write: test on the stick, from I/Q captures (noise floor, DC, image, a known station; no sound), whether leaving it out or changing it has an effect. Record register, value and measured effect in our own words. Drop the writes that have no effect. | **done 2026-11-25:** [radio-facts.md](radio-facts.md) (5 writes required, 2 gain-only, the rest no measurable effect; IF, rate and FIR formats confirmed by measurement; protocol details) |
| 3 | **Rewrite `rtl2832.go`'s setup part** (`initBaseband`, the demod write, `SetFIR`, `SetRate`, `setIF`, `Close`) in a fresh session that gets **only the fact list from step 2**, with no librtlsdr or kernel source in its context. The session that ran this audit has read librtlsdr and should not write it (a two-step clean room). Add a round-trip unit test for the FIR register packing. | **done 2026-11-26:** rewritten from the fact list in a fresh session; 7 setup writes, our 3.0 MHz IF, FIR/IF packing tests. On #2, 3.0 MHz stayed within 1.0 dB (station-to-mirror) and 0.4 dB (station-to-noise) of a same-session 3.57 MHz control; the audit's similarity check found only format-inherent constants. |
| 4 | **R820T facts by measurement.** Sweep the PLL to find our own lock range (lock bit, R2 bit 6), measure the bridge's longest write as was done for reads, and choose and justify our own IF (any IF inside the RTL2832U's passband works with `setIF`). | **done 2026-11-25:** lock 1716–3740 MHz, writes up to 15 registers, IF free from 2 to 4 MHz (radio-facts.md, D) |
| 5 | Re-run `provcheck.py`. `rtl2832.go` must show no constant runs beyond single register facts. Update this document with the result. | **done 2026-11-26:** section 10 |

Effort, roughly: steps 2 and 4 take about 6–8 % of a day (the stick on clock #2, silent
I/Q captures). Step 3 takes about 3–4 %. Steps 1 and 5 are small.

If this is not done, the radio stays out of the public release (TODO, release 1.7.0,
step 5).

## 8. Rules for radio code (since 2026-11-27, GPL-3.0-or-later)

These carry over into the contributor guide for new tuners (TODO, modular radio).

- **What may be used:**
  - datasheets and register descriptions
  - our own measurements
  - code under licences that combine with GPLv3: GPL-2.0-or-later (librtlsdr, its forks
    such as rtl-sdr-blog), GPLv3, LGPL, Apache-2.0, MIT, BSD, ISC
  - A language model's recollection of such code is fine too: no clean room is needed.
- **What may not:**
  - GPL-2.0-only code. Most Linux kernel drivers are GPL-2.0-only (`r820t.c`,
    `rtl2832.c`, `rtl2832_sdr.c`, `rtl28xxu.c`); check each file's licence line.
  - code with no licence or a non-free licence (e.g. CC BY-NC-ND)
- **Credit what you borrow:** a header in the file that names the project, its authors
  and its licence (as in `internal/rtlsdr/rtl2832.go`), plus a line in DETAILS
  "Credits" and the web UI's About card. Keep the original copyright lines when copying
  larger parts.
- **When unsure where code came from** (for example after a language model wrote it):
  run `provcheck.py` (section 9) against the likely sources. A match with a
  GPL-2.0-or-later project means "add the credit". A match with a GPL-2.0-only file
  means "rewrite or drop".
- **Testing a new tuner:** the driver must hold the sample rate after tuning and gain
  changes (`rtlfm`, the delivered S/s), the stream must run at real time (no stutter),
  and the station search and stereo/RDS must work. The I/Q captures and the `rtlfm
  facts-*` commands let a reviewer check this without owning the stick.

## 9. Re-running the check

    # reference sources (not part of the repo), e.g. into $TMPDIR/prov/src:
    #   osmocom/rtl-sdr: src/librtlsdr.c src/tuner_r82xx.c include/tuner_r82xx.h src/rtl_fm.c
    #   Linux: drivers/media/tuners/r820t.c, dvb-frontends/rtl2832.c, rtl2832_sdr.c,
    #          usb/dvb-usb-v2/rtl28xxu.c
    cd satellite
    python3 -I tools/provcheck/provcheck.py --ref $TMPDIR/prov/src \
        internal/rtlsdr/*.go internal/fmradio/demod.go internal/fmradio/decimate.go \
        internal/fmradio/rds.go internal/web/resample.go cmd/clockd/radio.go tools/rtlfm/*.go
    # noise level for comparison: the same on internal/output/flac.go, cmd/clockd/alarms.go

## 10. Re-check of the rewrite (2026-11-26, commit 2ff7c28, clockd 1.6.29)

**How it was written:** a fresh language-model session (GLM 5.4) with only
[radio-facts.md](radio-facts.md), the R820T2 register description and our own code. It
deleted the old `rtl2832.go` unread. Its statement: no librtlsdr or kernel source and
no register web searches. After the code was finished, a search showed it a few lines
of section 6 of this document; it says it did not use them, and the code was already
complete then.

**Similarity:** `provcheck.py` against the same reference versions (section 4, hashes
unchanged):
- The 14- and 8-constant init runs and the write-helper match are gone.
- What remains are runs of 4–5 constants that the data formats in facts C force on any
  implementation: the signed 8- and 12-bit tap limits (0x7f/0x80, 0x7ff/0x800) and the
  4/8-bit shifts of the FIR packing.
- Code shape is 1.1 % against librtlsdr and 0.1–0.6 % against the others, the same
  noise level as radio-free files.
- `r820t.go` is unchanged in substance (from the Rafael document, section 6).

**Structure:** a table of 7 setup writes, each citing its fact; one write function that
verifies every demodulator write by reading it back; IF and rate computed from their
formats; a FIR packer with a round-trip test. None of this follows the old code's
shape.

**Design values:**
- **IF:** 3.0 MHz, the writer's own choice (facts D).
- **VCO band:** 1800–3600 MHz, inside the measured lock range.
- **Tuner writes:** 15 registers per message, the measured limit.
- **Rate:** a final rewrite of the rate register's high word, which the writer found
  and documented by measurement (facts F).

**Function:** checked on clock #2.
- Ablation: the required writes still break the radio the measured way.
- Sample rates are within 0.2–0.5 %.
- A 30-minute silent playback ran without errors or lost blocks, and two station
  searches found 17–18 stations.
- Old against new driver, same session, alternating, 10 s each:
  - SUOMIPOP 106.3 MHz pilot: old 38.9–52.9 dB (median 46.3), new 38.3–68.1 dB (median
    47.4).
  - LOOP 99.1 MHz: 48.1–55.5 dB old, 51.8–56.3 dB new. LOOP's RDS name decoded with both.
  - SUOMIPOP's RDS name decoded with neither, so the transmitter side was the cause that
  night, not the rewrite.

**Verdict:** the radio code is our own: written from datasheets, our own measurements and
standards, or (for the RTL2832U) a measured fact list. It can be published under the
project licence. Remaining limits are those of radio-facts.md, section E: one stick
tested.

