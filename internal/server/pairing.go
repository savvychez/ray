package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Device is a paired phone (or other client), identified by its tailcat
// (WireGuard) node public key.
type Device struct {
	Key      string    `json:"key"`
	Name     string    `json:"name"`
	Added    time.Time `json:"added"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Pairing is an Authorizer that admits devices whose node key is in a
// JSON store, and admits (and remembers) a new device that presents the
// current one-time pairing code.
//
// The tailcat address itself already carries a WireGuard pre-shared key,
// so nobody without it can even reach the server. Pairing adds a second
// layer: a leaked address alone does not grant terminal access.
type Pairing struct {
	Path string // devices.json

	// PeerKey maps a connection to the peer's node key.
	PeerKey func(net.Conn) (string, bool)

	mu      sync.Mutex
	code    string
	expires time.Time
}

// NewCode generates and arms a fresh one-time pairing code.
func (p *Pairing) NewCode(ttl time.Duration) string {
	b := make([]byte, 16)
	rand.Read(b)
	code := strings.ToLower(strings.TrimRight(base32(b), "="))
	p.mu.Lock()
	p.code, p.expires = code, time.Now().Add(ttl)
	p.mu.Unlock()
	return code
}

// ClearCode disarms pairing.
func (p *Pairing) ClearCode() {
	p.mu.Lock()
	p.code = ""
	p.mu.Unlock()
}

func (p *Pairing) Authorize(conn net.Conn, hello Msg) error {
	key, ok := p.PeerKey(conn)
	if !ok {
		return errors.New("unknown peer")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	devs, err := p.loadLocked()
	if err != nil {
		return err
	}
	for i := range devs {
		if devs[i].Key == key {
			devs[i].LastSeen = time.Now()
			if hello.Name != "" {
				devs[i].Name = hello.Name
			}
			p.saveLocked(devs)
			return nil
		}
	}
	if hello.Pair == "" {
		return fmt.Errorf("%w: device not paired", ErrUnauthorized)
	}
	if p.code == "" || time.Now().After(p.expires) {
		return fmt.Errorf("%w: pairing code expired; run `ray pair` on the laptop", ErrUnauthorized)
	}
	if subtle.ConstantTimeCompare([]byte(hello.Pair), []byte(p.code)) != 1 {
		return fmt.Errorf("%w: wrong pairing code", ErrUnauthorized)
	}
	p.code = "" // one-time
	name := hello.Name
	if name == "" {
		name = "device"
	}
	devs = append(devs, Device{Key: key, Name: name, Added: time.Now(), LastSeen: time.Now()})
	return p.saveLocked(devs)
}

// Devices returns the paired devices.
func (p *Pairing) Devices() ([]Device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadLocked()
}

// Revoke removes devices whose key or name matches.
func (p *Pairing) Revoke(match string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	devs, err := p.loadLocked()
	if err != nil {
		return 0, err
	}
	var keep []Device
	for _, d := range devs {
		if d.Key == match || d.Name == match || (len(match) >= 8 && strings.Contains(d.Key, match)) {
			continue
		}
		keep = append(keep, d)
	}
	n := len(devs) - len(keep)
	if n > 0 {
		err = p.saveLocked(keep)
	}
	return n, err
}

func (p *Pairing) loadLocked() ([]Device, error) {
	b, err := os.ReadFile(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var devs []Device
	if err := json.Unmarshal(b, &devs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p.Path, err)
	}
	return devs, nil
}

func (p *Pairing) saveLocked(devs []Device) error {
	b, err := json.MarshalIndent(devs, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p.Path, b, 0o600)
}

// WriteFileAtomic writes via a temp file and rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func base32(b []byte) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no l, o, 0, 1
	var sb strings.Builder
	var acc, bits uint
	for _, c := range b {
		acc = acc<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(alphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(acc<<(5-bits))&31])
	}
	return sb.String()
}
