// Command ray exposes cmux terminals to a phone over tailcat.
//
//	ray serve      run the server (prints a pairing QR code)
//	ray pair       print a fresh pairing QR code for a running server
//	ray devices    list paired devices
//	ray revoke X   unpair a device by name or key
//	ray addr       print the server's tailcat address
//	ray tree       print the workspaces and surfaces cmux reports (debugging)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"

	"github.com/savvychez/ray/internal/cmux"
	"github.com/savvychez/ray/internal/server"
)

// DefaultAppURL is where the PWA is published (GitHub Pages).
const DefaultAppURL = "https://savvychez.github.io/ray/"

// rayPort is the tunneled TCP port the ray protocol listens on.
const rayPort = 1

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "pair":
		err = pair(args)
	case "devices":
		err = devices(args)
	case "revoke":
		err = revoke(args)
	case "addr":
		err = addr(args)
	case "tree":
		err = tree(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ray:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ray — drive cmux terminals from your phone, peer-to-peer over tailcat

usage:
  ray serve [flags]   run the server (prints a pairing QR code)
  ray pair            show a fresh pairing QR code for the running server
  ray devices         list paired devices
  ray revoke <name>   unpair a device
  ray addr            print this machine's tailcat address
  ray tree            show what cmux reports for each workspace (debugging)

run "ray serve -h" for server flags.
`)
}

func homeDir() string {
	if d := os.Getenv("RAY_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(h, ".config", "ray")
}

func keyPath() string     { return filepath.Join(homeDir(), "key.json") }
func devicesPath() string { return filepath.Join(homeDir(), "devices.json") }
func ctlPath() string     { return filepath.Join(homeDir(), "ctl.sock") }

// loadOrCreateKey returns a persistent tailcat identity so the address
// (and therefore the phone's saved connection) survives restarts.
func loadOrCreateKey(ctx context.Context, derpMapURL string) (*tailcat.PrivateKey, error) {
	pk := &tailcat.PrivateKey{}
	b, err := os.ReadFile(keyPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, pk); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", keyPath(), err)
		}
		if pk.Public.PresharedKey.IsZero() {
			return nil, fmt.Errorf("%s has no pre-shared key; delete it to regenerate", keyPath())
		}
		return pk, nil
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	pk = tailcat.NewPrivateKey()
	pk.Public.RegionID = -1 // auto-select the nearest relay
	ci := pk.Public
	if err := ci.Expand(ctx, tailcat.ExpandForServer, tailcat.DERPMapURL(derpMapURL)); err != nil {
		return nil, fmt.Errorf("selecting DERP region: %w", err)
	}
	pk.Public.RegionID = ci.Region[0].RegionID // pin it so the address is stable
	out, err := json.Marshal(pk)
	if err != nil {
		return nil, err
	}
	if err := server.WriteFileAtomic(keyPath(), out, 0o600); err != nil {
		return nil, err
	}
	return pk, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	demo := fs.Bool("demo", false, "serve fake terminals instead of cmux (commands run via sh -c!)")
	backendMode := fs.String("backend", "auto", "how to reach cmux: auto, socket, or cli")
	socketPath := fs.String("socket", cmux.DefaultSocketPath(), "cmux socket path")
	cliPath := fs.String("cmux", "", "path to the cmux CLI (default: search PATH and /Applications)")
	appURL := fs.String("app", envOr("RAY_APP_URL", DefaultAppURL), "URL of the ray PWA, used in pairing links")
	derpMapURL := fs.String("derpmap", tailcat.DefaultDERPMapURL, "DERP map URL")
	serveApp := fs.String("serve-app", "", "also serve this directory over plain HTTP (dev only), e.g. web/dist")
	httpAddr := fs.String("http", "127.0.0.1:8787", "listen address for -serve-app")
	pairTTL := fs.Duration("pair-ttl", 10*time.Minute, "how long a pairing code stays valid")
	tail := fs.Int("lines", 400, "scrollback lines streamed per terminal")
	verbose := fs.Bool("v", false, "verbose tailcat logging")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var backend server.Backend
	if *demo {
		backend = server.NewFakeBackend()
	} else {
		c, err := cmux.Dial(ctx, *backendMode, *socketPath, *cliPath)
		if err != nil {
			return fmt.Errorf("can't reach cmux (is it running? try -backend cli, or -demo):\n%w", err)
		}
		backend = &server.CmuxBackend{C: c}
	}
	log.Printf("cmux backend: %s", backend.Name())

	if *serveApp != "" {
		go func() {
			log.Printf("serving %s on http://%s", *serveApp, *httpAddr)
			log.Print(http.ListenAndServe(*httpAddr, http.FileServer(http.Dir(*serveApp))))
		}()
	}

	pk, err := loadOrCreateKey(ctx, *derpMapURL)
	if err != nil {
		return err
	}
	logf := logger.Discard
	if *verbose {
		logf = log.Printf
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	pairing := &server.Pairing{Path: devicesPath()}
	srv := &server.Server{Backend: backend, Auth: pairing, Hostname: host, DefaultTail: *tail}
	ts, region, err := startTailcat(ctx, pk, *derpMapURL, logf, srv, pairing)
	if err != nil {
		return err
	}
	defer ts.Close()
	address := string(pk.Public.Addr())
	log.Printf("tailcat relay region %d (%s)", region.RegionID, region.RegionName)

	link := func() string {
		code := pairing.NewCode(*pairTTL)
		return pairLink(*appURL, address, code, host)
	}

	// Control socket for `ray pair`.
	os.Remove(ctlPath())
	if ln, err := net.Listen("unix", ctlPath()); err != nil {
		log.Printf("control socket: %v (ray pair won't work)", err)
	} else {
		os.Chmod(ctlPath(), 0o600)
		defer os.Remove(ctlPath())
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				fmt.Fprintln(c, link())
				c.Close()
			}
		}()
		go func() { <-ctx.Done(); ln.Close() }()
	}

	devs, _ := pairing.Devices()
	fmt.Println()
	if len(devs) == 0 {
		fmt.Println("No devices paired yet. Scan this with your phone's camera:")
		printPairing(link(), *pairTTL)
	} else {
		fmt.Printf("ray is up. %d paired device(s). Run `ray pair` (or press Enter here) to pair another.\n", len(devs))
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				printPairing(link(), *pairTTL)
			}
		}()
	}

	<-ctx.Done()
	log.Print("shutting down")
	return nil
}

// startTailcat starts a tailcat server for identity pk whose port-1
// connections are served by srv, with pairing keyed on the peer's node key.
func startTailcat(ctx context.Context, pk *tailcat.PrivateKey, derpMapURL string, logf logger.Logf, srv *server.Server, pairing *server.Pairing) (*tailcat.Server, *tailcfg.DERPRegion, error) {
	ci := pk.Public
	if err := ci.Expand(ctx, tailcat.ExpandForServer, tailcat.DERPMapURL(derpMapURL)); err != nil {
		return nil, nil, fmt.Errorf("Expand: %w", err)
	}
	ts := &tailcat.Server{Key: pk.Private, PresharedKey: pk.Public.PresharedKey, Logf: logf, Region: ci.Region[0]}
	pairing.PeerKey = func(c net.Conn) (string, bool) {
		k, ok := ts.PeerKey(c.RemoteAddr())
		return k.String(), ok
	}
	ts.OnTCP = func(port uint16) func(net.Conn) {
		if port != rayPort {
			return nil
		}
		return srv.Serve
	}
	if err := ts.Start(); err != nil {
		ts.Close()
		return nil, nil, fmt.Errorf("starting tailcat: %w", err)
	}
	return ts, ci.Region[0], nil
}

func pairLink(app, address, code, host string) string {
	v := url.Values{}
	v.Set("c", address)
	if code != "" {
		v.Set("p", code)
	}
	v.Set("h", host)
	// The fragment never leaves the phone, so the address and code are not
	// sent to whoever hosts the PWA.
	return strings.TrimRight(app, "#") + "#" + v.Encode()
}

func printPairing(link string, ttl time.Duration) {
	fmt.Println()
	qrterminal.GenerateWithConfig(link, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         os.Stdout,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		QuietZone:      2,
	})
	fmt.Printf("\nor open on your phone:\n  %s\n\n(the pairing code works once and expires in %v)\n\n", link, ttl)
}

func pair(args []string) error {
	c, err := net.Dial("unix", ctlPath())
	if err != nil {
		return fmt.Errorf("is `ray serve` running? %w", err)
	}
	defer c.Close()
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	printPairing(strings.TrimSpace(line), 10*time.Minute)
	return nil
}

func devices(args []string) error {
	p := &server.Pairing{Path: devicesPath()}
	devs, err := p.Devices()
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no paired devices")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tADDED\tLAST SEEN\tKEY")
	for _, d := range devs {
		seen := "-"
		if !d.LastSeen.IsZero() {
			seen = d.LastSeen.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, d.Added.Local().Format("2006-01-02 15:04"), seen, shortKey(d.Key))
	}
	return tw.Flush()
}

func shortKey(k string) string {
	k = strings.TrimPrefix(k, "nodekey:")
	if len(k) > 16 {
		return k[:16]
	}
	return k
}

func revoke(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ray revoke <name|key>")
	}
	p := &server.Pairing{Path: devicesPath()}
	n, err := p.Revoke(args[0])
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no device matches %q", args[0])
	}
	fmt.Printf("revoked %d device(s); it will be refused on its next connection\n", n)
	return nil
}

func addr(args []string) error {
	b, err := os.ReadFile(keyPath())
	if err != nil {
		return fmt.Errorf("no key yet; run `ray serve` once: %w", err)
	}
	var pk tailcat.PrivateKey
	if err := json.Unmarshal(b, &pk); err != nil {
		return err
	}
	fmt.Println(pk.Public.Addr())
	return nil
}

func tree(args []string) error {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	backendMode := fs.String("backend", "auto", "how to reach cmux: auto, socket, or cli")
	socketPath := fs.String("socket", cmux.DefaultSocketPath(), "cmux socket path")
	cliPath := fs.String("cmux", "", "path to the cmux CLI")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := cmux.Dial(ctx, *backendMode, *socketPath, *cliPath)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Printf("backend: %s\n", c.Name())
	b := &server.CmuxBackend{C: c}
	wss, err := b.Tree(ctx)
	if err != nil {
		return err
	}
	for _, ws := range wss {
		front := ""
		if ws.Selected {
			front = "  (frontmost)"
		}
		fmt.Printf("\n%s  [%s]%s\n", ws.Title, ws.ID, front)
		if ws.Error != "" {
			fmt.Printf("  surface.list error: %s\n", ws.Error)
		} else if len(ws.Surfaces) == 0 {
			fmt.Println("  (no surfaces)")
		}
		for _, s := range ws.Surfaces {
			fmt.Printf("  - %-10s %s  [%s]\n", s.Type, s.Title, s.ID)
		}
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
