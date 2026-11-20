// vqed: LocalVQE as a pipe filter for clockd's local echo canceller.
//
//	vqed model.gguf
//
// Protocol (binary, little-endian, 16 kHz):
//
//	on start: writes "ok <hop>\n" once the model is loaded (or exits non-zero)
//	in:  blocks of <hop> mic int16 followed by <hop> reference int16
//	out: <hop> cleaned int16 per input block
//
// clockd decides when to use it (only while the speaker plays) and falls
// back to iot_audio_capture if this process fails. LocalVQE: Apache-2.0,
// github.com/localai-org/LocalVQE.
#include "localvqe_api.h"

#include <cstdio>
#include <cstdlib>
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

int main(int argc, char** argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: vqed model.gguf\n");
        return 2;
    }
    // the protocol gets the real stdout; the library's own prints go to stderr
    const int proto = dup(1);
    if (proto < 0 || dup2(2, 1) < 0) return 1;
    setenv("LOCALVQE_ALLOW_UNHASHED", "1", 0);  // the HF build of the model isn't on the allowlist
    localvqe_ctx_t ctx = localvqe_new(argv[1]);
    if (!ctx) {
        fprintf(stderr, "vqed: cannot load %s\n", argv[1]);
        return 1;
    }
    const int hop = localvqe_hop_length(ctx);
    char hello[32];
    int n = snprintf(hello, sizeof hello, "ok %d\n", hop);
    if (!writeFull(proto, hello, (size_t)n)) return 1;
    std::vector<int16_t> in(2 * hop), out(hop);
    while (readFull(0, in.data(), in.size() * 2)) {
        if (localvqe_process_frame_s16(ctx, in.data(), in.data() + hop, hop, out.data()) != 0) {
            fprintf(stderr, "vqed: %s\n", localvqe_last_error(ctx));
            return 1;
        }
        if (!writeFull(proto, out.data(), out.size() * 2)) break;
    }
    localvqe_free(ctx);
    return 0;
}
