// Package push sends Web Push notifications to paired phones: the one way
// to reach the PWA while iOS has it suspended, when the tailcat
// connection is gone. Payloads are end-to-end encrypted to the phone
// (RFC 8291); the push service (Apple's, for iOS) only relays them.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Notification is what the PWA's service worker shows.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Tag   string `json:"tag,omitempty"` // a newer notification with the same tag replaces it
	WS    string `json:"ws,omitempty"`  // terminal to open when tapped
	SF    string `json:"sf,omitempty"`
}

// Notifier keeps the server's VAPID key pair and the devices' push
// subscriptions in a JSON file (mode 0600) and sends to them.
type Notifier struct {
	Path    string // push.json
	Subject string // contact for the push service: an https URL or mailto:

	// Allowed reports whether a device may still be notified (it is still
	// paired). Nil allows all.
	Allowed func(device string) bool

	// Client sends the requests; nil uses a default with a timeout.
	Client *http.Client

	mu sync.Mutex
}

type state struct {
	VAPIDPublic  string         `json:"vapid_public"`
	VAPIDPrivate string         `json:"vapid_private"`
	Subs         map[string]sub `json:"subs"` // by device key
}

type sub struct {
	Name  string               `json:"name,omitempty"`
	Sub   webpush.Subscription `json:"sub"`
	Added time.Time            `json:"added"`
}

func (n *Notifier) load() (*state, error) {
	st := &state{Subs: map[string]sub{}}
	b, err := os.ReadFile(n.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("%s: %w", n.Path, err)
		}
		if st.Subs == nil {
			st.Subs = map[string]sub{}
		}
	}
	if st.VAPIDPublic == "" || st.VAPIDPrivate == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		st.VAPIDPublic, st.VAPIDPrivate = pub, priv
		st.Subs = map[string]sub{} // subscriptions were made for the old key
		if err := n.save(st); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (n *Notifier) save(st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.Path), 0o700); err != nil {
		return err
	}
	tmp := n.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.Path)
}

// PublicKey is the VAPID application server key phones subscribe with.
func (n *Notifier) PublicKey() (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st, err := n.load()
	if err != nil {
		return "", err
	}
	return st.VAPIDPublic, nil
}

// Subscribe stores device's push subscription (the browser's
// PushSubscription as JSON), replacing any earlier one.
func (n *Notifier) Subscribe(device, name string, subJSON []byte) error {
	var s webpush.Subscription
	if err := json.Unmarshal(subJSON, &s); err != nil {
		return fmt.Errorf("bad subscription: %w", err)
	}
	if s.Endpoint == "" || s.Keys.P256dh == "" || s.Keys.Auth == "" {
		return errors.New("bad subscription: missing endpoint or keys")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	st, err := n.load()
	if err != nil {
		return err
	}
	st.Subs[device] = sub{Name: name, Sub: s, Added: time.Now()}
	return n.save(st)
}

// Unsubscribe forgets device's subscription.
func (n *Notifier) Unsubscribe(device string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	st, err := n.load()
	if err != nil {
		return err
	}
	if _, ok := st.Subs[device]; !ok {
		return nil
	}
	delete(st.Subs, device)
	return n.save(st)
}

// Subscribed reports whether device has a subscription.
func (n *Notifier) Subscribed(device string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	st, err := n.load()
	if err != nil {
		return false
	}
	_, ok := st.Subs[device]
	return ok
}

// Send pushes msg to every subscribed device that skip doesn't exclude.
// Subscriptions the push service reports gone (404/410) are dropped. It
// returns how many deliveries were accepted.
func (n *Notifier) Send(ctx context.Context, msg Notification, skip func(device string) bool) (int, error) {
	n.mu.Lock()
	st, err := n.load()
	n.mu.Unlock()
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var sent int
	var errs []error
	var gone []string
	for dev, s := range st.Subs {
		if (n.Allowed != nil && !n.Allowed(dev)) || (skip != nil && skip(dev)) {
			continue
		}
		subj := s.Sub
		resp, err := webpush.SendNotificationWithContext(ctx, payload, &subj, &webpush.Options{
			HTTPClient:      client,
			Subscriber:      n.Subject,
			VAPIDPublicKey:  st.VAPIDPublic,
			VAPIDPrivateKey: st.VAPIDPrivate,
			TTL:             600,
			Urgency:         webpush.UrgencyHigh,
			Topic:           topic(msg.Tag),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			gone = append(gone, dev)
		case resp.StatusCode >= 300:
			errs = append(errs, fmt.Errorf("%s: push service: %s %s", s.Name, resp.Status, bytes.TrimSpace(body)))
		default:
			sent++
		}
	}
	if len(gone) > 0 {
		n.mu.Lock()
		if st, err := n.load(); err == nil {
			for _, d := range gone {
				delete(st.Subs, d)
			}
			n.save(st)
		}
		n.mu.Unlock()
	}
	return sent, errors.Join(errs...)
}

// topic lets the push service collapse undelivered notifications for the
// same terminal; it must be at most 32 URL-safe base64 characters.
func topic(tag string) string {
	out := make([]byte, 0, 32)
	for i := 0; i < len(tag) && len(out) < 32; i++ {
		c := tag[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}
