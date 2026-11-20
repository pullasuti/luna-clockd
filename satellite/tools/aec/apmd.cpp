// apmd: WebRTC audio processing (freedesktop webrtc-audio-processing, BSD-3) as a
// pipe filter for clockd: the same modules iot_audio_capture uses, open.
//
//	apmd [--ns 0-3|-1] [--no-aec] [--no-agc] [--aec-always]
//	     [--delay-ms N] [--aec-linear] [--aec-mask F] [--filter-blocks N]
//
// Experiments (off by default, see Engineering Notes):
//	--delay-ms N       the echo's delay behind the reference is known: AEC3 uses it
//	                   instead of searching for it
//	--aec-linear       AEC3's linear filter only (its suppressor passes everything:
//	                   the suppressor is what removes a voice under loud echo)
//	--aec-mask F       scale the suppressor's thresholds by F (>1 = suppresses less)
//	--filter-blocks N  length of the linear filter in 4 ms blocks (default 13)
//
// Protocol (binary, little-endian, 16 kHz mono), the same as vqed:
//
//	on start: "ok <hop>\n" (hop = 160, 10 ms)
//	in:  <hop> mic int16, then <hop> reference int16 (the speaker signal)
//	out: <hop> processed int16 per input block
//
// Processing: high-pass, AEC3 (fed with the reference), noise suppression
// (default very high, as iot_audio_capture), AGC2 adaptive digital + limiter.
// what the library's own build defines, for its internal AEC3 header
#define WEBRTC_APM_DEBUG_DUMP 0
#define WEBRTC_POSIX 1
#define WEBRTC_LINUX 1
#include <algorithm>
#include <cstdlib>
#include <memory>
#include <optional>
#include "api/scoped_refptr.h"
#include <webrtc/modules/audio_processing/include/audio_processing.h>
#include "api/audio/echo_canceller3_config.h"
#include "api/audio/echo_control.h"
#include "modules/audio_processing/aec3/echo_canceller3.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <unistd.h>
#include <vector>

static bool readFull(int fd, void* p, size_t n) {
    char* c = static_cast<char*>(p);
    while (n > 0) {
        ssize_t r = read(fd, c, n);
        if (r <= 0) return false;
        c += r;
        n -= (size_t)r;
    }
    return true;
}

static bool writeFull(int fd, const void* p, size_t n) {
    const char* c = static_cast<const char*>(p);
    while (n > 0) {
        ssize_t r = write(fd, c, n);
        if (r <= 0) return false;
        c += r;
        n -= (size_t)r;
    }
    return true;
}

// AEC3 with our own settings (the builder's default factory takes none).
struct Aec3Factory : webrtc::EchoControlFactory {
    webrtc::EchoCanceller3Config cfg;
    std::unique_ptr<webrtc::EchoControl> Create(int rate, int render, int capture) override {
        return std::make_unique<webrtc::EchoCanceller3>(cfg, std::nullopt, rate, render, capture);
    }
};

int main(int argc, char** argv) {
    int ns = 3, delayMs = -1, filterBlocks = 0;
    float mask = 1;
    bool aec = true, agc = true, aecAlways = false, linear = false;
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--ns") && i + 1 < argc) ns = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--no-aec")) aec = false;
        else if (!strcmp(argv[i], "--no-agc")) agc = false;
        else if (!strcmp(argv[i], "--aec-always")) aecAlways = true;
        else if (!strcmp(argv[i], "--delay-ms") && i + 1 < argc) delayMs = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--aec-linear")) linear = true;
        else if (!strcmp(argv[i], "--aec-mask") && i + 1 < argc) mask = (float)atof(argv[++i]);
        else if (!strcmp(argv[i], "--filter-blocks") && i + 1 < argc) filterBlocks = atoi(argv[++i]);
        else { fprintf(stderr, "apmd: unknown option %s\n", argv[i]); return 2; }
    }
    const int proto = dup(1);
    if (proto < 0 || dup2(2, 1) < 0) return 1;  // library prints go to stderr

    webrtc::AudioProcessingBuilder builder;
    if (delayMs >= 0 || linear || mask != 1 || filterBlocks > 0) {
        auto f = std::make_unique<Aec3Factory>();
        webrtc::EchoCanceller3Config& c = f->cfg;
        if (delayMs >= 0) {
            c.delay.use_external_delay_estimator = true;
            c.delay.default_delay = (size_t)(delayMs / 4);
        }
        if (linear) mask = 1e6f;
        if (mask != 1) {
            for (auto* t : {&c.suppressor.normal_tuning, &c.suppressor.nearend_tuning})
                for (auto* m : {&t->mask_lf, &t->mask_hf}) {
                    m->enr_transparent *= mask;
                    m->enr_suppress *= mask;
                    m->emr_transparent *= mask;
                }
        }
        if (filterBlocks > 0) {
            c.filter.refined.length_blocks = c.filter.refined_initial.length_blocks = (size_t)filterBlocks;
            c.filter.coarse.length_blocks = c.filter.coarse_initial.length_blocks = (size_t)filterBlocks;
        }
        builder.SetEchoControlFactory(std::move(f));
    }
    rtc::scoped_refptr<webrtc::AudioProcessing> apm = builder.Create();
    if (!apm) { fprintf(stderr, "apmd: cannot create APM\n"); return 1; }
    webrtc::AudioProcessing::Config cfg;
    cfg.high_pass_filter.enabled = true;
    cfg.echo_canceller.enabled = aec;
    cfg.echo_canceller.mobile_mode = false;
    if (ns >= 0) {
        cfg.noise_suppression.enabled = true;
        cfg.noise_suppression.level = (webrtc::AudioProcessing::Config::NoiseSuppression::Level)ns;
    }
    if (agc) {
        cfg.gain_controller2.enabled = true;
        cfg.gain_controller2.adaptive_digital.enabled = true;
    }
    apm->ApplyConfig(cfg);

    const int hop = 160;
    webrtc::StreamConfig sc(16000, 1);
    char hello[32];
    int n = snprintf(hello, sizeof hello, "ok %d\n", hop);
    if (!writeFull(proto, hello, (size_t)n)) return 1;
    std::vector<int16_t> in(2 * hop), out(hop);
    // The echo canceller only removes the clock's own sound (the reference), so
    // it runs only while the reference has sound and for a tail after it: in
    // silence it is about two thirds of apmd's CPU for nothing (same wake word
    // results with it off, measured 2026-11-05). --aec-always keeps it on.
    // 30 s after the last sound: room echo, and the gaps in a conversation (the user
    // speaking, HA thinking) so the canceller stays adapted between responses; a
    // fresh one removes little echo in its first second (a TTS start once
    // triggered the stop word after a 14 s gap with a 3 s tail)
    const int tailHops = 3000;
    const int refFloor = 4;    // |sample| above this counts as sound (about -78 dBFS)
    bool aecOn = aec;
    long quiet = aecAlways ? 0 : tailHops;  // hops since the reference last had sound
    if (aec && !aecAlways) {
        aecOn = false;
        cfg.echo_canceller.enabled = false;
        apm->ApplyConfig(cfg);
    }
    while (readFull(0, in.data(), in.size() * 2)) {
        if (aec && !aecAlways) {
            int peak = 0;
            for (int i = 0; i < hop; i++) peak = std::max(peak, std::abs((int)in[hop + i]));
            quiet = peak > refFloor ? 0 : quiet + 1;
            const bool want = quiet < tailHops;
            if (want != aecOn) {
                aecOn = want;
                cfg.echo_canceller.enabled = want;
                apm->ApplyConfig(cfg);
                fprintf(stderr, "apmd: echo canceller %s\n", want ? "on" : "off");
            }
        }
        if (aecOn) {
            apm->ProcessReverseStream(in.data() + hop, sc, sc, in.data() + hop);
            apm->set_stream_delay_ms(delayMs > 0 ? delayMs : 0);
        }
        if (apm->ProcessStream(in.data(), sc, sc, out.data()) != 0) {
            fprintf(stderr, "apmd: ProcessStream failed\n");
            return 1;
        }
        if (!writeFull(proto, out.data(), out.size() * 2)) break;
    }
    return 0;
}
