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

	"github.com/akleizor/ccmorph/util/paths"
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
// no dot, so ".<name>.adapter." names no other profile's files. The root is
// user-settable (CCMORPH_STATE_DIR), so it is quoted before the pattern: the
// scan takes the literal directory it was given, never whatever a `[` or a `*`
// in the path happens to match — the writer of these files uses the literal
// path, and a scan of a different directory than the write is how one root's
// launch deleted another's files (#848, #1020).
func AdapterFiles(root, name string) []string {
	files, _ := filepath.Glob(paths.GlobQuote(filepath.Join(root, "."+name+".adapter.")) + "*")
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

// sweepStaleAdapterKeys is a launch taking the dead keys of its profile with
// it: a core rebuild or a conf edit makes a new key, and the old key's empty
// lock and users files and its last log would otherwise pile up in the auth
// dir until rm or mv takes the profile (#1020). A key goes only under its own
// mutex, taken without waiting — a launch queued on it is a launch on that
// key — and only while nobody holds its users lock. Best effort in every
// direction: any doubt leaves the whole key for the next launch, and a
// session queued on a swept key's mutex finds the files gone and starts its
// own fresh adapter, the way the idle check leaves it.
func sweepStaleAdapterKeys(root, name, keep string) {
	prefix := "." + name + ".adapter."
	bases := map[string]bool{}
	for _, f := range AdapterFiles(root, name) {
		key := strings.TrimPrefix(filepath.Base(f), prefix)
		for _, s := range []string{".lock", ".users", ".log"} {
			key = strings.TrimSuffix(key, s) // one key's file, or its record
		}
		if !isAdapterKey(key) {
			continue // a prefix match no key ever made: not a file set to sweep
		}
		bases[AdapterBase(root, name, key)] = true
	}
	for base := range bases {
		if base != keep {
			sweepAdapterKey(base)
		}
	}
}

// adapterKeyLen is the length of the key AdapterKey makes; the shape check
// tracks its producer rather than a hand-written number.
var adapterKeyLen = len(AdapterKey("k", "m", "/ccmorph.no-conf", "/ccmorph.no-core"))

// isAdapterKey is the shape of an adapter key: lowercase hex, as long as
// AdapterKey truncates to. A file under the prefix whose key part is not that
// shape was written by no key — someone else's lock-shaped name — and the
// sweep leaves it however well its lock opens; only real keys are swept. A
// drift in AdapterKey's output is caught by the launches in this file's
// tests, whose real keys would stop being swept (#1020).
func isAdapterKey(key string) bool {
	if len(key) != adapterKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// sweepAdapterKey removes one dead key's file set, or leaves the key whole:
// its mutex only if it can be taken without waiting, its users lock only if a
// session does not hold it (one that cannot be read is held, as in othersHold),
// and its recorded adapter stopped only while the process table proves that
// pid runs this key's adapter — doubt is alive, as ever.
func sweepAdapterKey(base string) {
	mu, err := os.OpenFile(base+".lock", os.O_RDWR, 0) // no O_CREATE: a missing mutex is not one to sweep under
	if err != nil {
		return
	}
	defer mu.Close()
	if syscall.Flock(int(mu.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return // a launch or a release is on this key: it is alive
	}
	if users, err := os.OpenFile(base+".users", os.O_RDWR, 0); err == nil {
		held := syscall.Flock(int(users.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
		users.Close()
		if held {
			return // a session still uses this key
		}
	} else if !os.IsNotExist(err) {
		return // a lock that cannot be read is held: doubt never deletes
	}
	if raw, err := os.ReadFile(base); err == nil {
		if pid, _, ok := parseAdapterRecord(string(raw)); ok {
			yours, known := adapterOwns(pid, base)
			if !known {
				return // the table cannot answer for this pid: the next launch sweeps again
			}
			if yours {
				stopProcess(pid) // the adapter its sessions left behind, under its own mutex
			}
		}
	}
	for _, f := range []string{base, base + ".lock", base + ".users", base + ".log"} {
		os.Remove(f) // one key's file set; files already gone are swept
	}
}

// adapterOwns is whether pid runs this key's adapter, read from that pid's own
// argv argument by argument. known is false when the read cannot answer for a
// pid that exists; doubt is read as alive.
func adapterOwns(pid int, base string) (yours, known bool) {
	if pid <= 0 {
		return false, true
	}
	argv, ok := argvOfPID(pid)
	if !ok {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return false, true // the pid is gone: dead, not doubt
		}
		return false, false
	}
	return adapterArgvOwns(argv, base), true
}

// adapterArgvOwns is whether an argv, its argument boundaries kept, is the
// adapter command for base. Two shapes run the adapter, both proven by the
// command's own operands: spawnAdapter's
// `<core> adapter <profile> --auto [--model m] --shared <base>`, where a core
// that is a script gets the kernel's interpreter and script path in front of
// that, and the older `ccmorph-adapter <profile> ...`, which hop.go's
// adapterProfile already names and the zsh suite's core dispatcher runs by
// exec'ing it without the `adapter` word. Either command word is followed by
// its profile operand and its `--auto` operand, and base must be the exact
// operand of `--shared` after that span. The pair alone proves nothing — an
// unrelated `sh -c script junk --shared <base>` carries those two words as
// positionals the script never reads, and a sweep that stopped whatever
// merely spelled them killed a stranger (#1020).
func adapterArgvOwns(argv []string, base string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if !adapterCommandWord(argv, i) || argv[i+2] != "--auto" {
			continue // not this command: no adapter word, profile, --auto span
		}
		for j := i + 3; j+1 < len(argv); j++ {
			if argv[j] == "--shared" && argv[j+1] == base {
				return true
			}
		}
		return false
	}
	return false
}

// adapterCommandWord is whether argv[i] is how an adapter command is named.
// `adapter` is the core's subcommand, at any index behind the core's own path
// and interpreter. `ccmorph-adapter`, the older build's executable, is the
// program itself, so only the first or second word can be it — index 0 run
// directly, index 1 behind one interpreter. The bound keeps a stranger's
// script from promoting a later positional into the command: the base name
// must be exactly ccmorph-adapter, never a word that merely ends in it.
func adapterCommandWord(argv []string, i int) bool {
	return argv[i] == "adapter" || i <= 1 && filepath.Base(argv[i]) == "ccmorph-adapter"
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
	sweepStaleAdapterKeys(root, name, base)
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
