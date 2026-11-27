package util

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// usersPath is the users file this session holds; hold is the child that
	// names it in the process table for `ccmorph hop`.
	usersPath string
	hold      *sessionHold
}

// sessionHold is one session's marker on a shared adapter: a route-hold child
// on the adapter's users file, so the hop names the profile from the launcher's
// own children. in is the write end of the pipe the launcher keeps for its
// lifetime: when the launcher dies, the kernel closes it, the hold reads EOF,
// and no marker outlives the session that started it.
type sessionHold struct {
	pid int
	in  *os.File
}

// sessionHoldReady is how long a marker may take to land: it takes one shared
// lock, and no path holds the users file exclusively for longer than a
// non-blocking test.
const sessionHoldReady = 10 * time.Second

// startSessionHold spawns `<core> util route-hold <users> --exit-on-stdin-eof`
// and waits for its one readiness byte. The pipe's write end is returned with
// it: the hold can end only while this process lives, and when it dies the
// kernel closes that end.
func startSessionHold(core, users string) (*sessionHold, bool) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, false
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, false
	}
	cmd := exec.Command(core, "util", "route-hold", users, routeHoldExitStdin)
	cmd.Stdin, cmd.Stdout = inR, readyW
	// Its own session: a terminal signal to the launch's group must not end a
	// hold whose launch keeps running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		readyR.Close()
		readyW.Close()
		return nil, false
	}
	inR.Close()
	readyW.Close()
	go cmd.Wait() // reap it; the pipe says when it goes
	readyR.SetReadDeadline(time.Now().Add(sessionHoldReady))
	one := make([]byte, 1)
	_, err = io.ReadFull(readyR, one)
	readyR.Close()
	if err != nil || one[0] != '1' {
		inW.Close()
		cmd.Process.Kill()
		return nil, false
	}
	return &sessionHold{pid: cmd.Process.Pid, in: inW}, true
}

// endSessionHold ends this session's marker: TERM, then wait briefly for it to
// be gone. The PID is signalled only while the process table proves it is the
// core's route-hold on users (holdsRoute), so a number the system reused is
// never hit (#408). The wait watches the PID alone, not the users file: rm may
// have taken that path, and opening it would put back what rm removed.
func endSessionHold(holder int, users string) {
	if holder <= 0 || !holdsRoute(holder, users) {
		return
	}
	syscall.Kill(holder, syscall.SIGTERM)
	for range 200 { // ~2s
		if syscall.Kill(holder, 0) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdSession starts this session's marker on the adapter it holds. A session
// without one is exactly the session `ccmorph hop` can no longer name, so a
// marker that cannot land refuses the acquisition rather than silently losing
// the #257 guard (#1022).
func (a *SharedAdapter) holdSession(core, name string, errw io.Writer) bool {
	h, ok := startSessionHold(core, a.usersPath)
	if !ok {
		fmt.Fprintf(errw, "ccmorph: could not keep a hold on the shared adapter for '%s'; session not started.\n", name)
		return false
	}
	a.hold = h
	return true
}

// endHold stops this session's marker and closes the pipe the launcher kept.
func (a *SharedAdapter) endHold() {
	if a.hold == nil {
		return
	}
	endSessionHold(a.hold.pid, a.usersPath)
	a.hold.in.Close()
	a.hold = nil
}

// AdapterBase is the path a key's adapter state hangs off: the record, and
// base+".lock", base+".users" and base+".log" beside it. It is also the marker
// the adapter carries in its argv (`--shared <base>`), which is how an owner
// is proven.
func AdapterBase(root, name, key string) string {
	return filepath.Join(root, "."+name+".adapter."+key)
}

// adapterProfileFromUsers is the profile a shared adapter's users file names:
// <root>/.<name>.adapter.<key>.users, the shape AdapterBase builds. A profile
// name holds no dot, so the name is the segment between the leading dot and
// ".adapter."; any other path names no profile.
func adapterProfileFromUsers(path string) string {
	rest, ok := strings.CutSuffix(filepath.Base(path), ".users")
	if !ok || !strings.HasPrefix(rest, ".") {
		return ""
	}
	name, key, ok := strings.Cut(rest[1:], ".adapter.")
	if !ok || name == "" || key == "" || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
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
	a := &SharedAdapter{mutex: base + ".lock", state: base, usersPath: base + ".users", Log: base + ".log"}
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
				if !a.holdSession(core, name, errw) {
					users.Close()
					return a, false
				}
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
	if !a.holdSession(core, name, errw) {
		// The mutex kept every other session out, so this one is the only user:
		// the adapter it just started and recorded is nobody's.
		stopProcess(a.PID)
		if raw, err := os.ReadFile(base); err == nil {
			if pid, _, ok := parseAdapterRecord(string(raw)); ok && pid == a.PID {
				os.Remove(base)
			}
		}
		users.Close()
		return a, false
	}
	return a, true
}

// Release ends this session's use; the last user out stops the adapter. It
// never signals an adapter another session holds, and never a pid that does not
// run this one. This session's marker goes first, while the mutex keeps every
// acquire out: the last-user test below would see the marker's own shared lock
// otherwise, and a marker left behind would keep the adapter alive after its
// session died.
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
	a.endHold()
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
