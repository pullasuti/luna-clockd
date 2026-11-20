#!/bin/sh
# release.sh - build, stage and sign a clockd release package (build machine):
#
#	satellite/release.sh        -> satellite/build/clockd-<version>-arm64.tar.gz
#
# The version comes from cmd/clockd/main.go. Signing needs the private key
# made once with `go run ./tools/release keygen` (~/.config/clockd-release/,
# never in git); its public key must be in internal/release/keys.go.
# Install the package with: install-clock.sh -s SERIAL --release FILE
# (or on the clock: clockd -install FILE, then restart.sh).
set -e
S=$(cd "$(dirname "$0")" && pwd)
cd "$S"
V=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/clockd/main.go)
[ -n "$V" ] || { echo "no version in cmd/clockd/main.go"; exit 1; }
echo "== clockd $V: vet and test"
go vet ./... && go test -count=1 ./... >/dev/null
echo "== building"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o build/clockd-arm64 ./cmd/clockd
sh device/stage.sh | grep -v '^  '
echo "== signing"
OUT=build/clockd-$V-arm64.tar.gz
# the notes come from CHANGELOG.md (signed in the manifest): it needs an entry for $V
go run ./tools/release pack -version "$V" -dir build/clockd-device -o "$OUT" -notes ../CHANGELOG.md
go run ./tools/release verify "$OUT" >/dev/null && echo "verified: $OUT"
