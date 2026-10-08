# ray

Drive your [cmux](https://github.com/manaflow-ai/cmux) terminals from your phone.

`ray serve` runs on the Mac next to cmux. Your phone runs the ray PWA, a
Claude-style chat UI with your cmux workspaces and terminals in the sidebar.
The two talk peer-to-peer over [tailcat](https://github.com/tailscale/tailcat):
WireGuard encryption end to end, with no account, no control plane, and no
port forwarding.

```
 phone (PWA)                                         Mac
┌──────────────────┐    tailcat / WireGuard     ┌──────────────┐  v2 JSON-RPC  ┌──────┐
│ ray.wasm         │ ── via a DERP relay (WSS) ─▶│  ray serve   │ ─────────────▶│ cmux │
│ (tailcat in Go)  │◀── newline-delimited JSON ──│              │ socket or CLI │      │
└──────────────────┘                             └──────────────┘               └──────┘
```

## Quick start

```sh
# on the Mac (Go 1.27+; GOTOOLCHAIN=auto fetches it automatically)
go install github.com/savvychez/ray/cmd/ray@latest
ray serve
```

The first run creates a persistent identity in `~/.config/ray/key.json` and
prints a QR code. Scan it with the phone camera. It opens the PWA, which pairs
and connects. Then use **Share → Add to Home Screen** to install it.

- `ray pair`: print a new pairing QR for a running server. You can also press
  Enter in the `ray serve` terminal.
- `ray devices` / `ray revoke <name>`: list or unpair phones.
- `ray serve -demo`: fake terminals, for trying ray without cmux. Typed
  commands really run via `sh -c`.

### Reaching cmux

`ray serve -backend auto` (the default) first tries the cmux socket
(`$CMUX_SOCKET_PATH`, default `/tmp/cmux.sock`). If that fails, it falls back to
running `cmux rpc <method> -`. cmux's default socket mode, *cmux processes only*,
lets only processes started inside cmux connect, so you have two options:

- start `ray serve` from a cmux terminal (it then uses the socket), or
- start it anywhere else, for example from launchd. The CLI fallback works
  regardless. Set `CMUX_BIN` if `cmux` isn't on your `PATH` or in
  `/Applications/cmux.app`.

## Using the app

- **Sidebar**: your cmux workspaces, in cmux's groups (folders). Tap a group
  header to open its own terminals; the chevron folds it. The orange dot marks
  the workspace in front on the Mac, and **+** creates a workspace.
- **Tabs**: a workspace with several tabs shows them in a strip above the
  terminal, like cmux's tab bar.
- **Composer, line mode**: type a command or a prompt, then send. ray types the
  text into the terminal and presses Enter.
- **Composer, live mode**: tap `line` to switch to `live`. Every keystroke goes
  straight to the terminal, for TUIs like Claude Code, vim, or less.
- **Key bar**: esc, tab, a sticky ctrl, ^C, arrow keys, ⌫, ⏎, ^D, ^L, ^R, ^Z.
- **⋯ menu**: show this terminal on the Mac, wrap lines, text size, reconnect.
  You can also pinch the terminal to change its text size; the page itself
  doesn't zoom.
- **Versions**: the bottom of the sidebar shows the app's build (commit and
  build time) and the Mac's (`ray version`). When a new app release has
  downloaded, a banner offers to reload into it.

ray streams what cmux exposes through `surface.read_text`, which is plain text
with no colors. The view refreshes about every 250 ms while you're typing and
every 1.2 s when idle. Only changed lines go over the wire, and scrolling is
detected so it isn't resent as a full redraw.

### IBM Bob Shell

Terminals running [Bob Shell](https://bob.ibm.com) (`bob` / `bob chat`) get a
chat view on the phone. They're marked with a chat icon in the sidebar, and a
**Chat / Terminal** toggle sits above the view.

- **Conversation:** your messages and bob's replies render as chat, with
  markdown. Tool calls (commands, file edits, searches) are collapsible cards
  with their results. Bob's todo list shows as a checklist.
- **Status:** a pill shows *Working…*, *Needs your approval*, *Your turn* or
  *bob isn't running*, plus the session cost.
- **Approvals:** when bob asks for permission, the phone shows the request
  with **Approve / Always for this task / Reject**. ray presses the matching
  keys in bob's menu, and only if bob is actually waiting on a prompt.
- **Input:** the composer types into the same terminal, so it's the same
  session you'd see at your desk.

How it works: `ray serve` finds the `bob` process on each cmux terminal's tty
(`ps`, `lsof`). It then reads that session from bob's own store,
`~/.bob/db/bob.db` (SQLite, opened read-only), which bob updates as it goes.
Turn it off with `ray serve -bob=false`, or point at another database with
`-bob-db`.

## Security model

- The tailcat address in the QR code contains the server's WireGuard public key
  and a 256-bit pre-shared key. Without the address, nobody can even complete a
  handshake with the server.
- On top of that, devices are **paired**. Each phone keeps a WireGuard identity
  in localStorage. The QR code carries a one-time code that expires after 10
  minutes, and the server pins the phone's key the first time it connects. A
  leaked address alone gets refused. `ray revoke` removes a device; it is
  refused the next time it connects.
- The pairing data travels in the URL `#fragment`, so it never reaches the
  server hosting the PWA (GitHub Pages). The app clears it from the address
  bar right away.
- Browsers can't send raw UDP, so the phone always goes through a public
  tailcat DERP relay over WebSockets. The relay only sees WireGuard ciphertext.
  Public relays are best-effort and rate-limited.

## Development

```sh
make test       # unit tests + a real end-to-end test over a local DERP relay
make web        # build the PWA into web/dist (≈34 MB wasm, 7.6 MB gzipped)
make devstack   # local DERP + demo server + PWA; prints a pairing URL to open
```

Layout:

| Path | What |
|---|---|
| `cmd/ray` | CLI, tailcat server, pairing UX, e2e test |
| `internal/cmux` | cmux v2 RPC client (socket + `cmux rpc` CLI) |
| `internal/server` | session protocol, polling and diffing, pairing store, demo backend |
| `web/wasm` | Go→wasm bridge exposing `rayDial` (adapted from tailcat's web build) |
| `web/src` | the PWA: plain HTML, CSS, and JS, no build step besides the wasm |

### Hosting the PWA

`.github/workflows/pages.yml` publishes `web/dist` to GitHub Pages on every
push to `main`. Enable it under **Settings → Pages → Source: GitHub Actions**.
If you host it somewhere else, pass `ray serve -app https://your.host/` (or set
`RAY_APP_URL`) so the pairing links point there.

### Wire protocol

The phone opens one TCP stream to tunneled port 1 and exchanges
newline-delimited JSON frames, each tagged with a type field `t`:

| direction | frames |
|---|---|
| phone → Mac | `hello{name,pair?}` `watch{ws,sf}` `text{ws,sf,data}` `key{ws,sf,key}` `focus{ws,sf}` `new{}` `ping{}` |
| Mac → phone | `welcome{host}` `tree{workspaces}` `screen{ws,sf,drop,keep,lines}` `ack{id,error?}` `notice{error}` `error{code}` `pong{}` |

To apply a `screen` frame, the phone sets
`lines = prev.slice(drop, drop + keep).concat(frame.lines)`.
