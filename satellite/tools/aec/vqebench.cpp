// vqebench: run LocalVQE on raw 16 kHz s16 mono mic/ref files, frame by frame
// (as a live helper would), and report the time per 16 ms hop.
//
//	vqebench model.gguf mic.raw ref.raw out.raw
#include "localvqe_api.h"

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <vector>

static std::vector<int16_t> slurp(const char* p) {
    std::vector<int16_t> v;
    FILE* f = fopen(p, "rb");
    if (!f) { perror(p); exit(1); }
    int16_t buf[4096];
    size_t n;
    while ((n = fread(buf, 2, 4096, f)) > 0) v.insert(v.end(), buf, buf + n);
    fclose(f);
    return v;
}

int main(int argc, char** argv) {
    if (argc < 5) { fprintf(stderr, "usage: vqebench model.gguf mic.raw ref.raw out.raw\n"); return 2; }
    // localvqe_new detects the GTCRN line; the options path only loads graph models
    localvqe_ctx_t ctx;
    if (argc > 5) {
        localvqe_options_t o = localvqe_options_new();
        localvqe_options_set_model_path(o, argv[1]);
        localvqe_options_set_threads(o, atoi(argv[5]));
        ctx = localvqe_new_with_options(o);
        localvqe_options_free(o);
    } else {
        ctx = localvqe_new(argv[1]);
    }
    if (!ctx) { fprintf(stderr, "load failed\n"); return 1; }
    auto mic = slurp(argv[2]), ref = slurp(argv[3]);
    int hop = localvqe_hop_length(ctx);
    size_t n = std::min(mic.size(), ref.size()) / hop * hop;
    std::vector<int16_t> out(n);
    double worst = 0, total = 0;
    for (size_t i = 0; i < n; i += hop) {
        auto t0 = std::chrono::steady_clock::now();
        if (localvqe_process_frame_s16(ctx, &mic[i], &ref[i], hop, &out[i]) != 0) {
            fprintf(stderr, "process: %s\n", localvqe_last_error(ctx));
            return 1;
        }
        double ms = std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - t0).count();
        total += ms;
        if (i > 10 * (size_t)hop && ms > worst) worst = ms;
    }
    double hopms = 1000.0 * hop / 16000, frames = (double)n / hop;
    printf("%zu hops of %.0f ms: mean %.2f ms, worst %.2f ms per hop = %.0f %% of one core\n",
           (size_t)frames, hopms, total / frames, worst, 100 * total / frames / hopms);
    FILE* f = fopen(argv[4], "wb");
    fwrite(out.data(), 2, n, f);
    fclose(f);
    localvqe_print_profile(ctx);
    localvqe_free(ctx);
    return 0;
}
