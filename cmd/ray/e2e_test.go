package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"

	"github.com/savvychez/ray/internal/server"
)

var devStack = flag.Bool("devstack", false, "run TestDevStack: a local DERP + demo ray server that stays up for manual/browser testing")

// localStack runs a local DERP relay, a /derpmap.json for it, and a demo
// ray server reachable through it.
type localStack struct {
	derpMapURL string
	addr       string
	pairing    *server.Pairing
	web        *httptest.Server
}

func startLocalStack(t *testing.T, mux *http.ServeMux) *localStack {
	t.Helper()
	dm := integration.RunDERPAndSTUN(t, t.Logf, "127.0.0.1")
	if mux == nil {
		mux = http.NewServeMux()
	}
	mux.HandleFunc("/derpmap.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(dm)
	})
	web := httptest.NewServer(mux)
	t.Cleanup(web.Close)
	derpMapURL := web.URL + "/derpmap.json"

	pk := tailcat.NewPrivateKey()
	pk.Public.RegionID = -1
	ci := pk.Public
	ctx := context.Background()
	if err := ci.Expand(ctx, tailcat.ExpandForServer, tailcat.DERPMapURL(derpMapURL)); err != nil {
		t.Fatal(err)
	}
	pk.Public.RegionID = ci.Region[0].RegionID

	pairing := &server.Pairing{Path: filepath.Join(t.TempDir(), "devices.json")}
	fb := server.NewFakeBackend()
	srv := &server.Server{
		Backend: fb, Auth: pairing, Hostname: "testhost", Version: version(), DefaultTail: 200,
		Logf: t.Logf, ScreenFast: 50 * time.Millisecond, ScreenSlow: 100 * time.Millisecond, TreeEvery: 200 * time.Millisecond,
		Bob: fakeBob(t, fb),
	}
	ts, _, err := startTailcat(ctx, pk, derpMapURL, t.Logf, srv, pairing)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	return &localStack{derpMapURL: derpMapURL, addr: string(pk.Public.Addr()), pairing: pairing, web: web}
}

type testClient struct {
	t    *testing.T
	cl   *tailcat.Client
	conn net.Conn
	rd   *bufio.Reader
}

func (c *testClient) close() {
	c.conn.Close()
	c.cl.Close()
}

func dialStack(t *testing.T, st *localStack, priv key.NodePrivate) *testClient {
	t.Helper()
	cl := &tailcat.Client{Server: tailcat.Addr(st.addr), Key: priv, DERPMapURL: st.derpMapURL, Logf: t.Logf}
	t.Cleanup(func() { cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		pctx, pc := context.WithTimeout(ctx, 3*time.Second)
		_, err := cl.Ping(pctx)
		pc()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("ping: %v", err)
		}
	}
	c, err := cl.DialTCPPort(ctx, rayPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, cl: cl, conn: c, rd: bufio.NewReader(c)}
}

func (c *testClient) send(m server.Msg) {
	c.t.Helper()
	b, _ := json.Marshal(m)
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) recv() server.Msg {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	var m server.Msg
	if err := json.Unmarshal(line, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// recvUntil reads frames until pred matches one.
func (c *testClient) recvUntil(pred func(server.Msg) bool) server.Msg {
	c.t.Helper()
	for i := 0; i < 500; i++ {
		if m := c.recv(); pred(m) {
			return m
		}
	}
	c.t.Fatal("recvUntil: gave up")
	return server.Msg{}
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("needs local DERP")
	}
	st := startLocalStack(t, nil)
	phone := key.NewNode()

	// Unpaired device without a code is refused.
	c := dialStack(t, st, phone)
	c.send(server.Msg{T: "hello", Name: "phone"})
	if m := c.recv(); m.T != "error" || m.Code != "unauthorized" {
		t.Fatalf("unpaired hello: got %+v", m)
	}

	// Wrong code is refused; right code pairs.
	code := st.pairing.NewCode(time.Minute)
	c.close()
	c = dialStack(t, st, phone)
	c.send(server.Msg{T: "hello", Name: "phone", Pair: "nope"})
	if m := c.recv(); m.T != "error" {
		t.Fatalf("wrong code: got %+v", m)
	}
	c.close()
	c = dialStack(t, st, phone)
	c.send(server.Msg{T: "hello", Name: "phone", Pair: code})
	if m := c.recv(); m.T != "welcome" || m.Host != "testhost" {
		t.Fatalf("pair: got %+v", m)
	}
	tree := c.recvUntil(func(m server.Msg) bool { return m.T == "tree" })
	if len(tree.Workspaces) == 0 || len(tree.Workspaces[0].Surfaces) == 0 {
		t.Fatalf("empty tree: %+v", tree)
	}
	ws, sf := tree.Workspaces[0].ID, tree.Workspaces[0].Surfaces[0].ID

	// Watch, type a command, see output arrive as screen diffs.
	c.send(server.Msg{T: "watch", WS: ws, SF: sf})
	var screen []string
	apply := func(m server.Msg) {
		screen = append(append([]string{}, screen[m.Drop:m.Drop+*m.Keep]...), m.Lines...)
	}
	apply(c.recvUntil(func(m server.Msg) bool { return m.T == "screen" }))
	c.send(server.Msg{T: "text", ID: 7, WS: ws, SF: sf, Data: "echo hello-from-phone"})
	if m := c.recvUntil(func(m server.Msg) bool { return m.T == "ack" }); m.ID != 7 || m.Err != "" {
		t.Fatalf("ack: %+v", m)
	}
	c.send(server.Msg{T: "key", WS: ws, SF: sf, Key: "enter"})
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(strings.Join(screen, "\n"), "\nhello-from-phone\n") {
		if time.Now().After(deadline) {
			t.Fatalf("output never arrived; screen:\n%s", strings.Join(screen, "\n"))
		}
		apply(c.recvUntil(func(m server.Msg) bool { return m.T == "screen" }))
	}

	// The paired device reconnects without a code; the code was one-time.
	c.close()
	c = dialStack(t, st, phone)
	c.send(server.Msg{T: "hello", Name: "phone"})
	if m := c.recv(); m.T != "welcome" {
		t.Fatalf("reconnect: got %+v", m)
	}
	other := dialStack(t, st, key.NewNode())
	other.send(server.Msg{T: "hello", Name: "intruder", Pair: code})
	if m := other.recv(); m.T != "error" {
		t.Fatalf("code reuse: got %+v", m)
	}

	// Revoked devices are refused.
	if n, err := st.pairing.Revoke("phone"); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	c.close()
	c = dialStack(t, st, phone)
	c.send(server.Msg{T: "hello", Name: "phone"})
	if m := c.recv(); m.T != "error" {
		t.Fatalf("after revoke: got %+v", m)
	}
}

// TestDevStack keeps a local DERP and demo server running, serving the
// built PWA (web/dist) so it can be exercised in a desktop browser:
//
//	make web && go test ./cmd/ray -run TestDevStack -devstack -v -timeout 0
func TestDevStack(t *testing.T) {
	if !*devStack {
		t.Skip("pass -devstack")
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("../../web/dist")))
	st := startLocalStack(t, mux)
	code := st.pairing.NewCode(time.Hour)
	link := pairLink(st.web.URL+"/", st.addr, code, "devstack") + "&d=" + st.derpMapURL
	fmt.Println("DEVSTACK_URL", link)
	if f := os.Getenv("DEVSTACK_URL_FILE"); f != "" {
		os.WriteFile(f, []byte(link), 0o644)
	}
	select {}
}
