# Two-mic echo null with K frames of memory per frequency (learned on the first half of
# test/wwtest/music_stream0.wav, tested on the second half and on quiet_stream0.wav).
# usage (in test/wwtest): nullmic_k.py [K]
import sys, wave, numpy as np
K = int(sys.argv[1]) if len(sys.argv) > 1 else 4  # frames of memory per bin
sys.argv = ['x', 'music_stream0.wav']
exec(open(__file__.replace('nullmic_k.py', 'nullmic.py')).read().split("db = lambda")[0])

def feats(B):  # B[t], B[t-1] .. B[t-K+1] per bin
    return np.stack([np.vstack([np.zeros((k, B.shape[1]), complex), B[:len(B) - k]]) for k in range(K)], axis=-1)
def learnK(a):
    A, B = stft(a[:, 0]), stft(a[:, 1]); F = feats(B)
    e = (np.abs(A) ** 2).sum(1); sel = e > np.percentile(e, 30)
    W = np.zeros((A.shape[1], K), complex)
    for f in range(A.shape[1]):
        X = F[sel, f, :]; y = A[sel, f]
        W[f] = np.linalg.lstsq(X.T.conj() @ X + 1e-9 * np.eye(K), X.T.conj() @ y, rcond=None)[0]
    return W
def nullK(a, W):
    A, B = stft(a[:, 0]), stft(a[:, 1]); F = feats(B)
    return istft(A - (F * W[None]).sum(-1), len(a))
db = lambda x: 10 * np.log10(np.mean(x ** 2) + 1e-20)
m = read2('music_stream0.wav'); v = read2('quiet_stream0.wav')
# learn on the first half, test on the second (no in-sample cheating)
h = len(m) // 2
W = learnK(m[16000:h])
ym = nullK(m, W); yv = nullK(v, W)
E, YE, A, YA = stft(m[h:, 0]), stft(ym[h:]), stft(v[:, 0]), stft(yv)
for lo, hi in ((100, 500), (500, 1000), (1000, 2000), (2000, 4000), (4000, 8000)):
    b = slice(lo * N // 16000, hi * N // 16000)
    f = lambda X: 10 * np.log10((np.abs(X[:, b]) ** 2).mean() + 1e-20)
    print(f'  K={K} {lo:4d}-{hi:4d} Hz: echo -{f(E) - f(YE):5.1f} dB, voice -{f(A) - f(YA):5.1f} dB')
w = wave.open(f'null{K}.wav', 'wb'); w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
w.writeframes(np.clip(np.round(ym * 32767), -32768, 32767).astype('<i2').tobytes()); w.close()
