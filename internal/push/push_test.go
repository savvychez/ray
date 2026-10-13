package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func subJSON(t *testing.T, endpoint string) []byte {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	return b
}

func TestNotifier(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") || r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Errorf("not a VAPID/aes128gcm push: %v", r.Header)
		}
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "push.json")
	n := &Notifier{Path: path, Subject: "https://example.com/ray/", Client: srv.Client()}
	k1, err := n.PublicKey()
	if err != nil || k1 == "" {
		t.Fatalf("PublicKey: %q %v", k1, err)
	}
	if k2, _ := n.PublicKey(); k2 != k1 {
		t.Fatal("VAPID key not kept")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("push.json mode %v", fi.Mode().Perm())
	}
	if err := n.Subscribe("dev1", "iPhone", []byte(`{"endpoint":""}`)); err == nil {
		t.Error("accepted a subscription without keys")
	}
	if err := n.Subscribe("dev1", "iPhone", subJSON(t, srv.URL+"/a")); err != nil {
		t.Fatal(err)
	}
	if err := n.Subscribe("dev2", "old phone", subJSON(t, srv.URL+"/gone")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	msg := Notification{Title: "bob is done", Body: "It's macOS.", Tag: "bob-6F11D59D-B987"}
	sent, err := n.Send(ctx, msg, nil)
	if sent != 1 || err != nil {
		t.Fatalf("Send = %d, %v", sent, err)
	}
	if n.Subscribed("dev2") || !n.Subscribed("dev1") {
		t.Error("a gone subscription should be dropped, the live one kept")
	}
	if sent, _ := n.Send(ctx, msg, func(d string) bool { return d == "dev1" }); sent != 0 {
		t.Error("skip ignored")
	}
	n.Allowed = func(string) bool { return false } // unpaired
	if sent, _ := n.Send(ctx, msg, nil); sent != 0 {
		t.Error("sent to an unpaired device")
	}
	if err := n.Unsubscribe("dev1"); err != nil || n.Subscribed("dev1") {
		t.Errorf("Unsubscribe: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(hits, " ") != "/a /gone" && strings.Join(hits, " ") != "/gone /a" {
		t.Errorf("push service saw %v", hits)
	}
}

func TestTopic(t *testing.T) {
	if got := topic("bob-6F11D59D-B987-4475-8730-D05E83B501FE"); len(got) > 32 || !strings.HasPrefix(got, "bob-6F11D59D") {
		t.Errorf("topic = %q", got)
	}
}
