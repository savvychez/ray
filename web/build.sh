#!/bin/sh
# Builds the PWA into web/dist: static files + tailcat wasm (pre-gzipped,
# since GitHub Pages doesn't compress .wasm) + Go's wasm_exec.js.
set -eu
cd "$(dirname "$0")/.."
out=web/dist
rm -rf "$out"
mkdir -p "$out"
cp web/src/* "$out/"
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o "$out/ray.wasm" ./web/wasm
gzip -9 -c "$out/ray.wasm" > "$out/ray.wasm.gz"
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/"
version="$(git rev-parse --short HEAD 2>/dev/null || echo dev)-$(date +%s)"
sed -i.bak "s/__VERSION__/$version/" "$out/sw.js" && rm -f "$out/sw.js.bak"
touch "$out/.nojekyll"
echo "built $out ($(du -h "$out/ray.wasm.gz" | cut -f1) wasm.gz, version $version)"
