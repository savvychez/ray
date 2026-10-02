// Portions copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
//
// Adapted from github.com/tailscale/tailcat/web/main_js.go.

//go:build js && wasm

// Command wasm is the browser half of ray's tailcat transport. It exposes:
//
//	rayNewKey() => {privateKeyJSON, publicKey}
//	rayDial({addr, privateKey, derpMapURL?, port?, verbose?}) => Promise<conn>
//
// conn is {read(): Promise<Uint8Array|null>, write(u8): Promise, close()}.
// Browsers reach DERP relays over WebSockets, so all traffic is relayed
// (still end-to-end WireGuard encrypted).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"syscall/js"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func main() {
	js.Global().Set("rayNewKey", js.FuncOf(rayNewKey))
	js.Global().Set("rayPublicKey", js.FuncOf(rayPublicKey))
	js.Global().Set("rayDial", js.FuncOf(rayDial))
	if f := js.Global().Get("onRayWasmReady"); f.Type() == js.TypeFunction {
		f.Invoke()
	}
	select {}
}

// rayNewKey makes a persistent client identity for this device. The
// server pairs against its public key.
func rayNewKey(this js.Value, args []js.Value) any {
	pk := tailcat.PrivateKey{Private: key.NewNode()}
	b, err := json.Marshal(pk)
	if err != nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"privateKeyJSON": string(b),
		"publicKey":      pk.Private.Public().String(),
	})
}

// rayPublicKey returns the public key for a stored privateKeyJSON.
func rayPublicKey(this js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeString {
		return js.Null()
	}
	var pk tailcat.PrivateKey
	if err := json.Unmarshal([]byte(args[0].String()), &pk); err != nil {
		return js.Null()
	}
	return pk.Private.Public().String()
}

func rayDial(this js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeObject {
		return rejectedPromise(errors.New("rayDial requires an options object"))
	}
	opts := args[0]
	addr := optString(opts, "addr")
	derpMapURL := optString(opts, "derpMapURL")
	keyJSON := optString(opts, "privateKey")
	logf := logger.Discard
	if opts.Get("verbose").Truthy() {
		logf = log.Printf
	}
	port := uint16(1)
	if p := opts.Get("port"); p.Type() == js.TypeNumber {
		port = uint16(p.Int())
	}
	return makePromise(func() (any, error) {
		if addr == "" {
			return nil, errors.New("addr is required")
		}
		priv := key.NewNode()
		if keyJSON != "" {
			var pk tailcat.PrivateKey
			if err := json.Unmarshal([]byte(keyJSON), &pk); err != nil {
				return nil, fmt.Errorf("parsing privateKey: %w", err)
			}
			priv = pk.Private
		}
		cl := &tailcat.Client{
			Server:     tailcat.Addr(addr),
			Key:        priv,
			Logf:       logf,
			DERPMapURL: derpMapURL,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := pingUntil(ctx, cl); err != nil {
			cl.Close()
			return nil, err
		}
		c, err := cl.DialTCPPort(ctx, port)
		if err != nil {
			cl.Close()
			return nil, fmt.Errorf("DialTCPPort: %w", err)
		}
		return makeJSConn(c, func() { cl.Close() }), nil
	})
}

// pingUntil retries the handshake until it succeeds or ctx expires; the
// first pings can be lost while either side's DERP connection comes up.
func pingUntil(ctx context.Context, cl *tailcat.Client) error {
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := cl.Ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("can't reach server (is `ray serve` running?): %w", err)
		}
	}
}

func makeJSConn(c net.Conn, onClose func()) js.Value {
	buf := make([]byte, 64<<10)
	return js.ValueOf(map[string]any{
		"read": js.FuncOf(func(this js.Value, args []js.Value) any {
			return makePromise(func() (any, error) {
				n, err := c.Read(buf)
				if n > 0 {
					u8 := js.Global().Get("Uint8Array").New(n)
					js.CopyBytesToJS(u8, buf[:n])
					return u8, nil
				}
				if err == nil || errors.Is(err, io.EOF) {
					return js.Null(), nil
				}
				return nil, err
			})
		}),
		"write": js.FuncOf(func(this js.Value, args []js.Value) any {
			if len(args) != 1 {
				return rejectedPromise(errors.New("write requires a Uint8Array"))
			}
			b := make([]byte, args[0].Get("length").Int())
			js.CopyBytesToGo(b, args[0])
			return makePromise(func() (any, error) {
				if _, err := c.Write(b); err != nil {
					return nil, err
				}
				return js.Undefined(), nil
			})
		}),
		"close": js.FuncOf(func(this js.Value, args []js.Value) any {
			c.Close()
			if onClose != nil {
				onClose()
			}
			return nil
		}),
	})
}

func optString(v js.Value, name string) string {
	if p := v.Get(name); p.Type() == js.TypeString {
		return p.String()
	}
	return ""
}

func makePromise(f func() (any, error)) js.Value {
	handler := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			if res, err := f(); err == nil {
				resolve.Invoke(res)
			} else {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
			}
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

func rejectedPromise(err error) js.Value {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(err.Error()))
}
