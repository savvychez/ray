package bob

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Detector finds out whether a terminal (by tty) is running bob, and if so
// in which directory.
type Detector interface {
	Lookup(ctx context.Context, tty string) (dir string, ok bool)
}

// ProcDetector inspects processes with ps and lsof (macOS and Linux). One
// `ps` lists every process with its tty; lsof then runs only for bob
// processes, to find their working directory. Results are cached briefly
// since the sidebar is polled every few seconds.
type ProcDetector struct {
	TTL time.Duration

	mu    sync.Mutex
	at    time.Time
	byTTY map[string]string // tty → directory bob runs in
}

func (d *ProcDetector) Lookup(ctx context.Context, tty string) (string, bool) {
	tty = strings.TrimPrefix(tty, "/dev/")
	if tty == "" {
		return "", false
	}
	ttl := d.TTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byTTY == nil || time.Since(d.at) > ttl {
		d.byTTY = scanBob(ctx)
		d.at = time.Now()
	}
	dir, ok := d.byTTY[tty]
	return dir, ok
}

// scanBob maps each tty running bob to bob's working directory.
func scanBob(ctx context.Context) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,tty=,args=").Output()
	if err != nil {
		return map[string]string{}
	}
	return parsePS(ctx, out, processCwd)
}

// parsePS picks bob processes out of `ps -o pid=,tty=,args=` output.
func parsePS(ctx context.Context, out []byte, cwd func(context.Context, string) string) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[1] == "?" || f[1] == "??" || !IsBobCommand(f[2:]) {
			continue
		}
		tty := strings.TrimPrefix(f[1], "/dev/")
		if _, seen := m[tty]; seen {
			continue
		}
		if dir := cwd(ctx, f[0]); dir != "" {
			m[tty] = dir
			// macOS ps can print "s012" for /dev/ttys012; cmux reports the
			// full name, so index both spellings.
			if !strings.HasPrefix(tty, "tty") && !strings.HasPrefix(tty, "pts") {
				m["tty"+tty] = dir
			}
		}
	}
	return m
}

// IsBobCommand reports whether argv is bob's CLI: the `bob` launcher, or
// node running bobshell's dist/bob.js. Other bob-ish names (bob-status.py)
// don't count.
func IsBobCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	isBob := func(a string) bool {
		b := filepath.Base(a)
		return b == "bob" || b == "bob.js"
	}
	if isBob(argv[0]) {
		return true
	}
	if b := filepath.Base(argv[0]); b != "node" && !strings.HasPrefix(b, "node") {
		return false
	}
	// node [flags] script …: only the script decides.
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return isBob(a)
	}
	return false
}

func processCwd(ctx context.Context, pid string) string {
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", pid, "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

// StaticDetector maps ttys to directories; for tests and the demo.
type StaticDetector map[string]string

func (s StaticDetector) Lookup(ctx context.Context, tty string) (string, bool) {
	d, ok := s[tty]
	return d, ok
}
