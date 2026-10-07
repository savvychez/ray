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
commit="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then commit="$commit-dirty"; fi
built="$(date -u +%Y-%m-%dT%H:%MZ)"
version="$commit-$(date +%s)" # service worker cache key: new on every build
sed -i.bak "s/__VERSION__/$version/" "$out/sw.js" && rm -f "$out/sw.js.bak"
sed -i.bak -e "s/__APP_COMMIT__/$commit/" -e "s/__APP_BUILT__/$built/" "$out/app.js" && rm -f "$out/app.js.bak"
touch "$out/.nojekyll"
echo "built $out ($(du -h "$out/ray.wasm.gz" | cut -f1) wasm.gz, version $version)"
