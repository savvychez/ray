// Service worker: makes ray installable and start instantly (the wasm is
// ~8 MB gzipped, so it must only be downloaded once per release).
// VERSION is stamped by the build; a new deploy gets a new cache.
const VERSION = "__VERSION__";
const CACHE = "ray-" + VERSION;
const SHELL = [
  "./",
  "index.html",
  "app.js",
  "bob.js",
  "style.css",
  "wasm_exec.js",
  "manifest.webmanifest",
  "icon.svg",
  "icon-192.png",
  "icon-512.png",
  "ray.wasm.gz",
];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("ray-") && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  e.respondWith(
    caches.open(CACHE).then(async (c) => {
      // Pairing links carry their data in the #fragment, which never
      // reaches here, so "./" always matches the cached shell.
      const hit = await c.match(e.request, { ignoreSearch: true });
      if (hit) return hit;
      const resp = await fetch(e.request);
      if (resp.ok && url.pathname.startsWith(new URL("./", location).pathname)) c.put(e.request, resp.clone());
      return resp;
    }),
  );
});
