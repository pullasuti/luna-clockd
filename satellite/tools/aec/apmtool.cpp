// apmtool: WebRTC audio processing (AEC3 / NS / AGC) on 16 kHz WAV files,
// to rebuild what iot_audio_capture does with open code and compare it.
//
//	apmtool [options] mic.wav out.wav [ref.wav]
//	  --pre DB      gain before processing (the raw PDM mic is ~-87 dBFS)
//	  --ns N        noise suppression 0-3 (low..very high), -1 off
//	  --agc1 MODE   off | digital | fixed   (GainController1)
//	  --agc2        GainController2 adaptive digital (+limiter)
//	  --fixed DB    GainController2 fixed gain
//	  --hpf         high-pass filter
//	  --ch N        use only mic channel N (default: all channels of mic.wav)
//
// mic.wav: 16-bit, 1 or 2 channels; ref.wav: 16-bit mono (speaker signal).
// Built against freedesktop webrtc-audio-processing (BSD-3).
#include "api/scoped_refptr.h"
#include <webrtc/modules/audio_processing/include/audio_processing.h>

#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

struct Wav {
    int ch = 0, rate = 0;
    std::vector<int16_t> s;
};

static Wav readWav(const char* p) {
    Wav w;
    FILE* f = fopen(p, "rb");
    if (!f) { perror(p); exit(1); }
    std::vector<char> b;
    char buf[65536];
    size_t n;
    while ((n = fread(buf, 1, sizeof buf, f)) > 0) b.insert(b.end(), buf, buf + n);
    fclose(f);
    size_t i = 12;
    while (i + 8 <= b.size()) {
        uint32_t sz;
        memcpy(&sz, &b[i + 4], 4);
        if (!memcmp(&b[i], "fmt ", 4)) {
            uint16_t c; uint32_t r;
            memcpy(&c, &b[i + 10], 2); memcpy(&r, &b[i + 12], 4);
            w.ch = c; w.rate = (int)r;
        } else if (!memcmp(&b[i], "data", 4)) {
            size_t m = std::min<size_t>(sz, b.size() - i - 8) / 2;
            w.s.resize(m);
            memcpy(w.s.data(), &b[i + 8], m * 2);
            break;
        }
        i += 8 + sz + (sz & 1);
    }
    return w;
}

static void writeWav(const char* p, const std::vector<int16_t>& s, int ch, int rate) {
    FILE* f = fopen(p, "wb");
    uint32_t data = (uint32_t)s.size() * 2, riff = 36 + data, fmt = 16, br = rate * ch * 2;
    uint16_t pcm = 1, c = (uint16_t)ch, ba = (uint16_t)(ch * 2), bits = 16;
    fwrite("RIFF", 1, 4, f); fwrite(&riff, 4, 1, f); fwrite("WAVEfmt ", 1, 8, f);
    fwrite(&fmt, 4, 1, f); fwrite(&pcm, 2, 1, f); fwrite(&c, 2, 1, f); fwrite(&rate, 4, 1, f);
    fwrite(&br, 4, 1, f); fwrite(&ba, 2, 1, f); fwrite(&bits, 2, 1, f);
    fwrite("data", 1, 4, f); fwrite(&data, 4, 1, f); fwrite(s.data(), 2, s.size(), f);
    fclose(f);
}

int main(int argc, char** argv) {
    double pre = 0, fixed = 0;
    int ns = -1, only = -1;
    std::string agc1 = "off";
    bool agc2 = false, hpf = false;
    std::vector<const char*> pos;
    for (int i = 1; i < argc; i++) {
        std::string a = argv[i];
        auto next = [&]() { if (i + 1 >= argc) { fprintf(stderr, "%s needs a value\n", a.c_str()); exit(2); } return argv[++i]; };
        if (a == "--pre") pre = atof(next());
        else if (a == "--ns") ns = atoi(next());
        else if (a == "--agc1") agc1 = next();
        else if (a == "--agc2") agc2 = true;
        else if (a == "--fixed") fixed = atof(next());
        else if (a == "--hpf") hpf = true;
        else if (a == "--ch") only = atoi(next());
        else pos.push_back(argv[i]);
    }
    if (pos.size() < 2) { fprintf(stderr, "usage: apmtool [options] mic.wav out.wav [ref.wav]\n"); return 2; }
    Wav mic = readWav(pos[0]);
    Wav ref;
    if (pos.size() > 2) ref = readWav(pos[2]);
    if (mic.rate != 16000 || (pos.size() > 2 && (ref.rate != 16000 || ref.ch != 1))) {
        fprintf(stderr, "need 16 kHz input (ref mono)\n");
        return 2;
    }
    int ch = only >= 0 ? 1 : mic.ch;
    size_t frames = mic.s.size() / mic.ch;
    // pre-gain and channel selection
    std::vector<int16_t> in(frames * ch);
    const double g = std::pow(10.0, pre / 20);
    for (size_t f = 0; f < frames; f++)
        for (int c = 0; c < ch; c++) {
            double v = mic.s[f * mic.ch + (only >= 0 ? only : c)] * g;
            in[f * ch + c] = (int16_t)std::max(-32768.0, std::min(32767.0, std::round(v)));
        }

    rtc::scoped_refptr<webrtc::AudioProcessing> apm = webrtc::AudioProcessingBuilder().Create();
    webrtc::AudioProcessing::Config cfg;
    cfg.echo_canceller.enabled = pos.size() > 2;
    cfg.echo_canceller.mobile_mode = false;
    cfg.high_pass_filter.enabled = hpf;
    if (ns >= 0) {
        cfg.noise_suppression.enabled = true;
        cfg.noise_suppression.level = (webrtc::AudioProcessing::Config::NoiseSuppression::Level)ns;
    }
    if (agc1 != "off") {
        cfg.gain_controller1.enabled = true;
        cfg.gain_controller1.mode = agc1 == "fixed" ? webrtc::AudioProcessing::Config::GainController1::kFixedDigital
                                                   : webrtc::AudioProcessing::Config::GainController1::kAdaptiveDigital;
    }
    if (agc2 || fixed > 0) {
        cfg.gain_controller2.enabled = true;
        cfg.gain_controller2.adaptive_digital.enabled = agc2;
        cfg.gain_controller2.fixed_digital.gain_db = (float)fixed;
    }
    apm->ApplyConfig(cfg);
    webrtc::StreamConfig sc(16000, ch), rc(16000, 1);
    const size_t blk = 160;
    std::vector<int16_t> out(in.size());
    std::vector<int16_t> rb(blk);
    for (size_t f = 0; f + blk <= frames; f += blk) {
        if (cfg.echo_canceller.enabled) {
            for (size_t k = 0; k < blk; k++) rb[k] = f + k < ref.s.size() ? ref.s[f + k] : 0;
            apm->ProcessReverseStream(rb.data(), rc, rc, rb.data());
            apm->set_stream_delay_ms(0);
        }
        if (apm->ProcessStream(&in[f * ch], sc, sc, &out[f * ch]) != 0) {
            fprintf(stderr, "ProcessStream failed\n");
            return 1;
        }
    }
    // mono output: channel 0 (APM writes the processed result to every channel)
    std::vector<int16_t> mono(frames);
    for (size_t f = 0; f < frames; f++) mono[f] = out[f * ch];
    writeWav(pos[1], mono, 1, 16000);
    return 0;
}
