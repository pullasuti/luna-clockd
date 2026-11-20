# The microphone pipeline, and why there is only one

The web UI used to offer three echo cancellers (Original, WebRTC, LocalVQE). Since
clockd v0.84.4 there is one pipeline and no choice. This page records what was measured
and why. Day-by-day detail is in `Engineering Notes.md` (sections dated 2026-11-03 and
2026-11-10).

## What runs

    raw PDM mics -> two-mic echo null -> apmd (WebRTC: AEC3, noise suppression, AGC2) -> wake word
                         ^                   ^
                         learned             clockd's own output mix as the echo reference

- **Two-mic null** (`internal/localmic/null.go`): learns how the speaker reaches the two
  microphones and subtracts one from the other. Removes 27-30 dB of the clock's own sound.
- **apmd**: our 64-bit build of the open WebRTC audio processing, the same modules the
  vendor's `iot_audio_capture` is built on.
- **The dip**: while music plays, a wake score of 0.15 or more turns the music to 12 % for
  4 s, so a repeated (or late-scoring) wake word is heard. Part of the setting "Easier
  while the clock plays sound".

## How it was measured

- **Rig** (`satellite/tools/wwtest`): recorded wake and stop words played from a second
  speaker at the clock, in a quiet room and over music from the clock at 38 % volume;
  counted are words detected at the live thresholds.
- **Offline** (`satellite/cmd/micbench`): the rig's captures replayed on a PC through the
  same code. The echo canceller's starting state moves one capture by 0-3 words of 10, so
  variants are compared by their range over 8 starting points, never by one run.

One room, one music track, one voice set: the numbers rank the options against each
other, they are not a general hit rate.

## Results

| | Quiet room (of 10) | Over music (of 10) | Notes |
|---|---|---|---|
| Original (vendor) | 4-7 | 0 | closed vendor binary |
| WebRTC + null | 5-8 | 0 at the threshold, 4 above 0.5 | stop word over music 4 of 5 (0 before the long null) |
| WebRTC + null + dip | 4-6 | 2-3 | the shipped pipeline |
| LocalVQE 2.7K + null | (not run) | same range as WebRTC | offline only |
| LocalVQE 49k + null | (not run) | same range as WebRTC | offline only; 30 % too slow for the clock's CPU |

The rig's clips are marginal even in quiet (4 to 8 of 10 from run to run), so differences
of one or two words mean nothing.

## Reasoning

1. **The null does the work.** It takes out about 30 dB, which is the limit set by how
   alike the two microphones hear the speaker. On one raw microphone without it, no
   canceller scored anything (all words 0.00-0.15).
2. **The canceller behind the null barely matters.** What the null leaves is the part
   the reference signal does not explain. AEC3 finds 3-7 dB more in it, LocalVQE 49k 3 dB,
   LocalVQE 2.7K 1 dB. All three give the same wake word counts within their run-to-run
   spread. An earlier result favouring the 49k model was with a shorter, weaker null.
3. **So the simplest canceller wins.** WebRTC is the smallest, fastest and best
   understood of the three, and it also brings noise suppression and gain. LocalVQE
   added a second helper, a model file and a build chain for no measured gain; the 49k
   model would also have needed work to run in real time.
4. **First-try wakes over music come from the dip**, not from cancelling harder.
5. **The vendor's processing is the fallback only.** It hears a quiet room about as well
   and the wake word over music not at all, and the aim of the project is not to depend on it. It is kept
   because a clock that cannot hear is worse than one that uses it.

## What is left in place

- **Automatic fallback**: if apmd is missing or fails, clockd starts `iot_audio_capture`
  again and the Microphone page says why. clockd exiting also hands the microphones back.
- **By hand** (developers, the test rig): `voice.mic_source` in the config or
  `POST /api/config` still takes `webrtc` or `vendor`. Flag file
  `/data/local/clockd/use-iot_audio` keeps the vendor service running from boot.
- **LocalVQE**: disabled, not deleted. `tools/aec/build-vqed.sh` builds it; with
  `aec/vqed`, `aec/model.gguf` and the flag file `/data/local/clockd/use-localvqe` on the
  clock, it runs in place of apmd.

## Not covered by the tests

- Other rooms, music, voices and distances; talk radio or TV as the sound source.
- False dips from vocals in the music (none noticed in the first days of use).
- A real voice instead of recorded clips.
