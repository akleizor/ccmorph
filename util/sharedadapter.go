package util

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A gateway profile's adapter is shared by every session that asks for the same
// thing: one adapter per (profile, model, conf content, core binary), however
// many sessions run on it. The mechanism is the shared bridge's: a mutex file, a
// users file every session holds LOCK_SH on, and a record naming the adapter.
// The last session out stops the adapter; one that dies without releasing —
// SIGKILL, a crash — leaves an adapter that notices, by itself, that nobody holds
// the users file any more and exits (SharedAdapterIdle).

// SharedAdapter is one session's use of a shared adapter.
type SharedAdapter struct {
	PID     int
	Port    string // "" for a native profile, which runs no adapter
	Native  bool
	Refused bool   // the adapter's own refusal was the whole message
	Log     string // the adapter's stable log
	mutex   string
	state   string
	users   *os.File
}

// AdapterBase is the path a key's adapter state hangs off: the record, and
// base+".lock", base+".users" and base+".log" beside it. It is also the marker
// the adapter carries in its argv (`--shared <base>`), which is how an owner
// is proven.
func AdapterBase(root, name, key string) string {
	return filepath.Join(root, "."+name+".adapter."+key)
}

// AdapterFiles are every shared-adapter file of a profile under root, whatever
// its key: rm deletes and mv moves them with the profile. A profile name holds
// no dot, so ".<name>.adapter." names no other profile's files.
func AdapterFiles(root, name string) []string {
	files, _ := filepath.Glob(filepath.Join(root, "."+name+".adapter.*"))
	return files
}

// AdapterKey is everything that makes two adapters identical: the profile, the
// bare model id, the profile's conf (a rotated key or base_url is a new
// adapter) and the core binary (an upgrade is a new adapter).
func AdapterKey(name, model, confPath, core string) string {
	h := sha256.New()
	conf, _ := os.ReadFile(confPath)
	identity := core
	if fi, err := os.Stat(core); err == nil {
		identity += fmt.Sprintf(" %d %d", fi.ModTime().UnixNano(), fi.Size())
	}
	for _, part := range []string{name, strings.SplitN(model, "[", 2)[0], string(conf), identity} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// adapterOwns is whether pid runs an adapter started with `--shared base`,
// read from that pid's own argv argument by argument. known is false when the
// read cannot answer for a pid that exists; doubt is read as alive.
func adapterOwns(pid int, base string) (yours, known bool) {
	if pid <= 0 {
		return false, true
	}
	if argv, ok := argvOfPID(pid); ok {
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--shared" && argv[i+1] == base {
				return true, true
			}
		}
		return false, true
	}
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false, true
	}
	return false, false
}

func parseAdapterRecord(raw string) (pid int, port string, ok bool) {
	f := strings.Fields(raw)
	if len(f) != 2 {
		return 0, "", false
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n <= 0 || f[1] == "" {
		return 0, "", false
	}
	return n, f[1], true
}

// stopProcess is TERM, 12 s for the process to stop its own child, then KILL.
func stopProcess(pid int) {
	if pid <= 0 {
		return
	}
	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; alive() && i < 240; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if alive() {
		syscall.Kill(pid, syscall.SIGKILL)
		for i := 0; alive() && i < 40; i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// AcquireAdapter returns the running adapter for (name, model) or starts one,
// and holds this session's use of it until Release. The result's Port/Native/
// Refused say what `util adapter-start` would have printed; ok is false when
// the adapter did not start (its reason is already on errw).
func AcquireAdapter(core, root, name, model, confPath string, errw io.Writer) (*SharedAdapter, bool) {
	base := AdapterBase(root, name, AdapterKey(name, model, confPath, core))
	a := &SharedAdapter{mutex: base + ".lock", state: base, Log: base + ".log"}
	if os.MkdirAll(root, 0o700) != nil {
		fmt.Fprintf(errw, "ccmorph: '%s': could not create %s.\n", name, root)
		return a, false
	}
	mu, err := lockFile(base+".lock", syscall.LOCK_EX)
	if err != nil {
		fmt.Fprintf(errw, "ccmorph: could not lock %s: %v\n", base+".lock", err)
		return a, false
	}
	defer mu.Close()
	users, err := lockFile(base+".users", syscall.LOCK_SH)
	if err != nil {
		fmt.Fprintf(errw, "ccmorph: could not lock %s: %v\n", base+".users", err)
		return a, false
	}
	if raw, err := os.ReadFile(base); err == nil {
		if pid, port, ok := parseAdapterRecord(string(raw)); ok {
			if yours, known := adapterOwns(pid, base); yours || !known {
				a.PID, a.Port, a.users = pid, port, users
				return a, true
			}
		}
		os.Remove(base) // a record of an adapter that is gone
	}
	os.Remove(a.Log) // one adapter's lifetime of log, never an ever-growing file
	var out bytes.Buffer
	code := spawnAdapter(core, name, model, []string{"--shared", base}, true, a.Log, &out, errw)
	for _, line := range strings.Split(out.String(), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch {
		case line == Refused:
			a.Refused = true
		case k == "PID":
			a.PID, _ = strconv.Atoi(v)
		case k == "PORT":
			a.Port = v
		case line == "NATIVE=1":
			a.Native = true
		}
	}
	if code != 0 {
		users.Close()
		return a, false
	}
	if a.Native {
		// The one this call just started, which nothing needs: no session joins a
		// native profile's adapter, so nothing records it.
		stopProcess(a.PID)
		users.Close()
		return a, true
	}
	if err := os.WriteFile(base, []byte(fmt.Sprintf("%d %s", a.PID, a.Port)), 0o600); err != nil {
		// An adapter nobody recorded would be started again by the next launch.
		stopProcess(a.PID)
		users.Close()
		fmt.Fprintf(errw, "ccmorph: could not record the adapter for '%s' in %s (%v); stopped it.\n", name, base, err)
		return a, false
	}
	a.users = users
	return a, true
}

// Release ends this session's use; the last user out stops the adapter. It
// never signals an adapter another session holds, and never a pid that does not
// run this one.
func (a *SharedAdapter) Release() {
	if a == nil || a.users == nil {
		return
	}
	// No O_CREATE: rm takes these files with the profile, and a session that
	// outlives it must not put them back.
	if f, err := os.OpenFile(a.mutex, os.O_RDWR, 0); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		defer f.Close()
	}
	fd := int(a.users.Fd())
	syscall.Flock(fd, syscall.LOCK_UN)
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		if yours, known := adapterOwns(a.PID, a.state); known {
			if yours {
				stopProcess(a.PID)
			}
			if raw, err := os.ReadFile(a.state); err == nil {
				if pid, _, ok := parseAdapterRecord(string(raw)); ok && pid == a.PID {
					os.Remove(a.state)
				}
			}
		}
	}
	a.users.Close()
	a.users = nil
}

// SharedAdapterIdle is the shared adapter's own check, run on its poll: true
// once no session holds the users file, after taking its record out (only a
// record that names pid). It runs under the mutex, so a session joining at
// this moment either holds the file already or waits for the record's removal
// and starts its own. A mutex it cannot take is a start or a stop in progress:
// not idle yet. A file it cannot read is held: doubt never ends an adapter. Files that are
// gone (rm, mv) end it.
func SharedAdapterIdle(base string, pid int) bool {
	mu, err := os.OpenFile(base+".lock", os.O_RDWR, 0)
	if err != nil {
		// rm and mv take the files with the profile: an adapter whose files are
		// gone has no sessions to wait for. Any other failure is doubt.
		return os.IsNotExist(err)
	}
	defer mu.Close()
	if syscall.Flock(int(mu.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false
	}
	if othersHold(base + ".users") {
		return false
	}
	if raw, err := os.ReadFile(base); err == nil {
		if rpid, _, ok := parseAdapterRecord(string(raw)); ok && rpid == pid {
			os.Remove(base)
		}
	}
	return true
}
