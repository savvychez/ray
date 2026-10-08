package bob

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Detector finds out whether a cmux terminal is running bob, and if so in
// which directory. Terminals are identified by cmux surface id, with their
// tty (when cmux reports one) as a fallback.
type Detector interface {
	Lookup(ctx context.Context, surfaceID, tty string) (Proc, bool)
}

// Proc is a running bob process.
type Proc struct {
	PID     string
	TTY     string // as ps prints it ("ttys012", "s012", "pts/3"); "" if none
	Args    string
	Dir     string // working directory; "" if unknown
	Surface string // CMUX_SURFACE_ID from its environment, upper-cased
	Start   time.Time
	Resume  string // task id from -r/--resume, if bob was started that way
}

// ProcDetector inspects processes with ps (and lsof on macOS). One `ps`
// lists every process; only bob processes are then looked at more closely,
// for their working directory and the cmux surface they were started in.
// Results are cached briefly since the sidebar is polled every few seconds.
type ProcDetector struct {
	TTL time.Duration

	mu    sync.Mutex
	at    time.Time
	procs []Proc
}

func (d *ProcDetector) Lookup(ctx context.Context, surfaceID, tty string) (Proc, bool) {
	surfaceID = strings.ToUpper(surfaceID)
	tty = strings.TrimPrefix(tty, "/dev/")
	procs := d.Procs(ctx)
	// cmux sets CMUX_SURFACE_ID in each terminal, and bob inherits it: the
	// surest link between a process and its terminal.
	for _, p := range procs {
		if p.Dir != "" && surfaceID != "" && p.Surface == surfaceID {
			return p, true
		}
	}
	if tty == "" {
		return Proc{}, false
	}
	for _, p := range procs {
		if p.Dir != "" && p.Surface == "" && sameTTY(p.TTY, tty) {
			return p, true
		}
	}
	return Proc{}, false
}

// Procs lists running bob processes (cached for TTL).
func (d *ProcDetector) Procs(ctx context.Context) []Proc {
	ttl := d.TTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.at.IsZero() || time.Since(d.at) > ttl {
		d.procs = ScanProcs(ctx)
		d.at = time.Now()
	}
	return d.procs
}

// sameTTY compares tty names, allowing for macOS ps printing "s012" for
// /dev/ttys012.
func sameTTY(a, b string) bool {
	return a != "" && (a == b || "tty"+a == b || a == "tty"+b)
}

// ScanProcs finds bob processes, with their working directory and cmux
// surface.
func ScanProcs(ctx context.Context) []Proc {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,tty=,lstart=,args=").Output()
	if err != nil {
		return nil
	}
	procs := parsePS(out)
	for i := range procs {
		procs[i].Dir = processCwd(ctx, procs[i].PID)
		procs[i].Surface = processSurface(ctx, procs[i].PID)
	}
	return procs
}

// parsePS picks bob processes out of `ps -o pid=,tty=,lstart=,args=`
// output. lstart is five fields, like "Thu Oct  8 13:58:01 2026".
func parsePS(out []byte) []Proc {
	var procs []Proc
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || !IsBobCommand(f[7:]) {
			continue
		}
		tty := strings.TrimPrefix(f[1], "/dev/")
		if tty == "?" || tty == "??" {
			tty = ""
		}
		start, _ := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[2:7], " "), time.Local)
		procs = append(procs, Proc{PID: f[0], TTY: tty, Args: strings.Join(f[7:], " "), Start: start, Resume: resumeArg(f[8:])})
	}
	return procs
}

// resumeArg is the task id given to bob with -r/--resume, if any.
func resumeArg(args []string) string {
	for i, a := range args {
		switch {
		case (a == "-r" || a == "--resume") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			return args[i+1]
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume=")
		}
	}
	return ""
}

// IsBobCommand reports whether argv is bob's CLI: the `bob` launcher, or
// node running bobshell's code. Other bob-ish names (bob-status.py) don't
// count.
func IsBobCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	isBob := func(a string) bool {
		b := filepath.Base(a)
		return b == "bob" || b == "bob.js" || strings.Contains(a, "/bobshell/")
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
	if runtime.GOOS == "linux" {
		dir, _ := os.Readlink("/proc/" + pid + "/cwd")
		return dir
	}
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

var surfaceEnv = regexp.MustCompile(`(?:^|[\s\x00])CMUX_SURFACE_ID=([0-9A-Fa-f-]{36})`)

// processSurface reads CMUX_SURFACE_ID from a process's environment.
func processSurface(ctx context.Context, pid string) string {
	var env []byte
	if runtime.GOOS == "linux" {
		env, _ = os.ReadFile("/proc/" + pid + "/environ")
	} else {
		// macOS: -E appends the environment to the command (own processes).
		env, _ = exec.CommandContext(ctx, "ps", "-E", "-ww", "-o", "command=", "-p", pid).Output()
	}
	if m := surfaceEnv.FindSubmatch(env); m != nil {
		return strings.ToUpper(string(m[1]))
	}
	return ""
}

// StaticDetector maps surface ids or ttys to directories; for tests and the
// demo.
type StaticDetector map[string]string

func (s StaticDetector) Lookup(ctx context.Context, surfaceID, tty string) (Proc, bool) {
	d, ok := s[surfaceID]
	if !ok {
		d, ok = s[tty]
	}
	return Proc{Dir: d}, ok
}
