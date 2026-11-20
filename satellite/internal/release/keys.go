package release

// trustedKeys are the Ed25519 public keys (base64) clockd accepts release
// packages from. The private key lives on the build machine only
// (tools/release keygen). To replace a key, add the new one, ship a release
// signed with the old one, then drop the old one in a later release.
var trustedKeys = []string{
	"f4y9d8syEQfUcYdJ1k+1P/uflhjgdWCY3u8+kXlf3sc=", // build machine, 2026-11-09
}
