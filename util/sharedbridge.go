package util

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrBridgeNotStarted is AcquireBridge's answer when start failed; it has
// already printed its own reason.
var ErrBridgeNotStarted = errors.New("bridge did not start")

// SharedBridge is a provider profile's one cliproxyapi, used by every session of
// that profile. OpenAI refresh tokens are single-use and cliproxyapi joins
// concurrent refreshes only inside one process, so two bridges on one auth
// directory send the same refresh token and the second gets
// refresh_token_reused, which signs the profile out (#250).
type SharedBridge struct {
	PID, Port, Key, TmpDir string
	mutex, state           string
	users                  *os.File // held shared while this session uses the bridge
}

func lockFile(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// probeBridge is whether a bridge answers its listing; a test swaps in a probe
// that never answers to model a slow bridge.
var probeBridge = listsModel

// bridgeProbeTries bounds how often a launch asks a recorded bridge whether it
// serves before the record counts as dead.
const bridgeProbeTries = 3

// BridgeFiles are a profile's bridge state under root: the handle, the
// start/stop mutex and the users' shared locks. rm deletes and mv moves them
// with the profile.
func BridgeFiles(root, name string) []string {
	base := filepath.Join(root, "."+name+".bridge")
	return []string{base, base + ".lock", base + ".users"}
}

// parseHandle reads a recorded handle: PID PORT KEY TMPDIR, split at the first
// three spaces so a TMPDIR holding one still reads whole.
func parseHandle(raw string) (pid, port, key, tmp string, ok bool) {
	f := strings.SplitN(strings.TrimSuffix(raw, "\n"), " ", 4)
	if len(f) != 4 {
		return "", "", "", "", false
	}
	for _, v := range f {
		if v == "" {
			return "", "", "", "", false
		}
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n <= 0 {
		return "", "", "", "", false
	}
	return f[0], f[1], f[2], f[3], true
}

// othersHold is whether a session other than this one still holds the users
// lock. Call it under the mutex and before taking this session's own shared
// lock: flock is per open file, so our lock would answer yes. A lock file that
// cannot be read is held: a launch must never start a second bridge on doubt.
func othersHold(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// AcquireBridge returns the profile's running bridge, or starts one with start.
// Files <root>/.<name>.bridge{,.lock,.users} hold its handle, the start/stop
// mutex and the users' shared locks. Ok is false when start failed; err names
// any other refusal in words the launch prints.
func AcquireBridge(root, name string, start func() (pid, port, key, tmp string, ok bool)) (*SharedBridge, bool, error) {
	files := BridgeFiles(root, name)
	base := files[0]
	mu, err := lockFile(base+".lock", syscall.LOCK_EX)
	if err != nil {
		return nil, false, fmt.Errorf("could not lock %s: %w", base+".lock", err)
	}
	defer mu.Close()
	held := othersHold(files[2])
	users, err := lockFile(base+".users", syscall.LOCK_SH)
	if err != nil {
		return nil, false, fmt.Errorf("could not lock %s: %w", files[2], err)
	}
	b := &SharedBridge{mutex: base + ".lock", state: base, users: users}
	if raw, err := os.ReadFile(base); err == nil {
		pid, port, key, tmp, ok := parseHandle(string(raw))
		if ok && answers(pid, port, key, tmp, held) {
			b.PID, b.Port, b.Key, b.TmpDir = pid, port, key, tmp
			return b, true, nil
		}
		if ok {
			stopIfOurs(pid, tmp) // a stuck bridge would still refresh the token
		} else if held {
			users.Close()
			return nil, false, fmt.Errorf("the bridge record %s cannot be read while another session of '%s' uses it; quit that session, or remove the file once none runs", base, name)
		}
	}
	pid, port, key, tmp, ok := start()
	if !ok {
		users.Close()
		return nil, false, ErrBridgeNotStarted
	}
	b.PID, b.Port, b.Key, b.TmpDir = pid, port, key, tmp
	if werr := os.WriteFile(base, []byte(strings.Join([]string{pid, port, key, tmp}, " ")), 0o600); werr != nil {
		// A bridge nobody recorded must not outlive the launch: the next launch
		// would find no handle and start a second on the same auth dir.
		n, _ := strconv.Atoi(pid)
		BridgeStop(n, tmp, filepath.Join(root, name))
		os.Remove(base)
		users.Close()
		return nil, false, fmt.Errorf("could not record the bridge for '%s' in %s (%v); stopped it, so no second bridge shares its login", name, base, werr)
	}
	return b, true, nil
}

// argvOfPID reads the argv the ownership checks use; a test swaps in a reader
// that cannot answer for a pid, as an unreadable table or a row without argv
// does.
var argvOfPID = argvOf

// owns is whether pid still runs the bridge whose config lives in tmp, read
// from that pid's own argv, never a reused PID: one read of one process, not
// a walk of every process on the machine. known is false when the read cannot
// answer for pid: no argv was read while the pid exists, as an unreadable table
// or a row without arguments gives. A pid that is certainly gone is dead;
// doubt is read as alive, the way othersHold treats a lock it cannot read.
func owns(pid int, tmp string) (yours, known bool) {
	if pid <= 0 || tmp == "" {
		return false, true
	}
	argv, ok := argvOfPID(pid)
	if ok {
		return strings.Contains(strings.Join(argv, " "), filepath.Join(tmp, "config.yaml")), true
	}
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false, true // the pid is gone: dead, not doubt
	}
	return false, false
}

// routeHoldExitStdin is the operand that ends a route hold when its stdin
// reaches EOF: a session's shared-adapter hold is spawned with the write end of
// a pipe only its launcher keeps, so a launcher that dies without releasing
// closes it and the marker goes with its session (#1022).
const routeHoldExitStdin = "--exit-on-stdin-eof"

// routeHoldUsers is the users file a core route-hold process is holding, read
// from one record of that pid, never a reused PID: the kernel names its
// executable ccmorph-core and its argv, argument by argument, is
// <dir>/ccmorph-core util route-hold <users>[, --exit-on-stdin-eof], as the
// launch spawned it. The executable is the proof — not argv[0], which the
// program chooses — and the argument boundaries are kept: the joined command
// line ps prints can not tell one operand reading "/x/ccmorph-core util
// route-hold <users>" from the three words it spells, so a hold on some other
// file would pass for this route's. Flagged is true for the shape a session's
// shared-adapter marker takes. No readable record, no proof (#408).
func routeHoldUsers(pid int) (users string, flagged, ok bool) {
	if pid <= 0 {
		return "", false, false
	}
	exe, argv, ok := imageOf(pid)
	if !ok || len(argv) == 0 || filepath.Base(exe) != launcherComm ||
		filepath.Base(argv[0]) != launcherComm {
		return "", false, false
	}
	switch {
	case len(argv) == 4 && argv[1] == "util" && argv[2] == "route-hold":
		return argv[3], false, true
	case len(argv) == 5 && argv[1] == "util" && argv[2] == "route-hold" && argv[4] == routeHoldExitStdin:
		return argv[3], true, true
	}
	return "", false, false
}

// holdsRoute is whether pid still runs the core's route hold on users: the
// proof is routeHoldUsers', and the users file must be this one. No users file,
// no proof (#408).
func holdsRoute(pid int, users string) bool {
	if pid <= 0 || users == "" {
		return false
	}
	got, _, ok := routeHoldUsers(pid)
	return ok && got == users
}

// retirable is the PID a retirement may signal for the bridge its handle
// names: pid while the process table shows it running that bridge, 0 once it
// is gone, so the auth files and the rendered config are still cleaned up.
// Ok is false while pid still runs something the table does not show as that
// bridge — a PID the system reused, perhaps another session's, or a bridge
// whose argv the table could not read: nothing of it is the retirement's to
// touch, and the next acquire probes the record (#408).
func retirable(pid, tmp string) (n int, ok bool) {
	n, _ = strconv.Atoi(pid)
	if yours, _ := owns(n, tmp); yours {
		return n, true
	}
	if n > 0 && syscall.Kill(n, 0) != syscall.ESRCH {
		return 0, false
	}
	return 0, true
}

// answers is whether the recorded bridge serves. One failed probe proves
// nothing while the bridge still runs its config and another session holds it:
// joining a slow bridge keeps one refresher, while killing it and starting a
// second signs the profile out (#294). Without a holder alive, a bounded retry
// is what says dead; a table that could not answer for the PID is not death.
func answers(pid, port, key, tmp string, held bool) bool {
	n, _ := strconv.Atoi(pid)
	doubt := false
	for i := 0; i < bridgeProbeTries; i++ {
		if probeBridge(port, key) {
			return true
		}
		yours, known := owns(n, tmp)
		if known && !yours {
			return false // dead or a reused PID: nothing to wait for
		}
		if held {
			return true
		}
		doubt = !known
	}
	// Doubt is not death: a table that could not answer never makes a launch
	// start a second bridge on the same auth dir.
	return doubt
}

// stopIfOurs stops a recorded bridge only while that PID still runs the
// cliproxyapi whose config lives in tmp, never a reused PID. A read that
// cannot answer proves neither death nor ownership, so it stops nothing and
// leaves the config for the bridge that may still run it.
func stopIfOurs(pid, tmp string) {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 || tmp == "" {
		return
	}
	yours, known := owns(n, tmp)
	if yours {
		BridgeStop(n, tmp, "")
	} else if known {
		if _, err := os.Stat(tmp); err == nil && strings.Contains(filepath.Base(tmp), "ccmorph-bridge-") {
			os.RemoveAll(tmp)
		}
	}
}

// recordNames is whether the record at path names the bridge whose rendered
// config lives in tmp: its TMPDIR field, split as parseHandle splits it. Each
// start makes a fresh TMPDIR (MkdirTemp), so it names one bridge for good,
// where a PID can be reused and a port or key can repeat. A record that is
// missing, unreadable, half written or names another bridge is not this
// bridge's to remove.
func recordNames(path, tmp string) bool {
	raw, err := os.ReadFile(path)
	if err != nil || tmp == "" {
		return false
	}
	f := strings.SplitN(strings.TrimSuffix(string(raw), "\n"), " ", 4)
	return len(f) == 4 && f[3] == tmp
}

// Release ends this session's use; the last user stops the bridge.
func (b *SharedBridge) Release(authDir string) {
	if b == nil || b.users == nil {
		return
	}
	// No O_CREATE: rm takes these files with the profile, and a session that
	// outlives it must not put them back.
	if f, err := os.OpenFile(b.mutex, os.O_RDWR, 0); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		defer f.Close()
	}
	fd := int(b.users.Fd())
	syscall.Flock(fd, syscall.LOCK_UN)
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		pid, _ := strconv.Atoi(b.PID)
		yours, known := owns(pid, b.TmpDir)
		// A read that cannot answer proves nothing: the bridge may still run,
		// so its record and config stay for the next launch to join or stop.
		// Reading doubt as death here cleared the PID, skipped the stop and
		// orphaned a live bridge (#409).
		if known {
			if !yours {
				pid = 0 // a bridge that died left its PID to whatever the system started next
			}
			BridgeStop(pid, b.TmpDir, authDir)
			// The record goes only while it names this bridge. Once rm has taken
			// this bridge's files, a fresh launch may have made new ones and
			// recorded its own bridge at the same path; removing that record
			// leaves the fresh bridge running unrecorded, and the next launch
			// starts a second on the same auth dir (#250, GH-837).
			if recordNames(b.state, b.TmpDir) {
				os.Remove(b.state)
			}
		}
	}
	b.users.Close()
	b.users = nil
}

// RetireRoute stops the bridge a route's handle names, but only once nothing
// else uses it. The route's holder kept its use while the route ran, so ending
// that hold is retirement's first act and the stop belongs to the last user
// out — a session, or the last route (#300). The handle may also name a bridge
// a replacement has already superseded: the record, not the handle, says which
// bridge the profile runs now, and a retirement must not take the
// replacement's record with the bridge it names. The bridge and the holder
// are signalled only while the process table shows each still running its
// own, so a PID the system reused is never stopped (#408).
func RetireRoute(authDir, pid, tmp, holder string) {
	if authDir == "" { // a handle without an auth directory names no bridge files
		EndRouteHold(holder, "")
		if n, ok := retirable(pid, tmp); ok {
			BridgeStop(n, tmp, "")
		}
		return
	}
	root, name := filepath.Dir(authDir), filepath.Base(authDir)
	files := BridgeFiles(root, name)
	EndRouteHold(holder, files[2])
	if mu, err := lockFile(files[1], syscall.LOCK_EX); err == nil {
		defer mu.Close()
	}
	if othersHold(files[2]) {
		return
	}
	// A record naming another bridge means the handle's died and something
	// started a replacement that the record now owns: stopping what the handle
	// names could hit a reused PID, and removing the record would leave the
	// replacement running beside the next acquire's bridge on one auth
	// directory (#300). The handle's own leftovers are still its to take.
	if raw, err := os.ReadFile(files[0]); err == nil {
		if rpid, _, _, rtmp, ok := parseHandle(string(raw)); ok && (rpid != pid || rtmp != tmp) {
			stopIfOurs(pid, tmp)
			return
		}
	}
	n, ok := retirable(pid, tmp)
	if !ok {
		return
	}
	BridgeStop(n, tmp, authDir)
	os.Remove(files[0])
}

// routeHold is `util route-hold <users-file> [--exit-on-stdin-eof]`: the holder
// behind a route's ownership of the shared bridge it runs on. It takes the same
// shared lock a session of the profile holds and keeps it until it is signaled,
// so the bridge's stop owners — the last session out, a sibling route's
// retirement — see the route's use and leave the bridge running. The hold dies
// with the process: the kernel closes the lock.
//
// With --exit-on-stdin-eof it also ends when its stdin reaches EOF. A shared
// adapter's session marker is spawned that way, with the write end of a pipe
// only its launcher keeps, so a launcher that dies without releasing closes it
// and the marker goes with its session (#1022).
func routeHold(args []string, in io.Reader, out, errw io.Writer) int {
	watch := false
	switch {
	case len(args) == 2 && args[1] == routeHoldExitStdin:
		watch = true
	case len(args) == 1:
	default:
		fmt.Fprintln(errw, "usage: ccmorph-core util route-hold <users-file> ["+routeHoldExitStdin+"]")
		return 2
	}
	f, err := lockFile(args[0], syscall.LOCK_SH)
	if err != nil {
		return 1
	}
	defer f.Close()
	fmt.Fprint(out, "1") // the hold has landed
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	if !watch {
		<-c
		return 0
	}
	eof := make(chan struct{})
	if in == nil {
		<-c
		return 0
	}
	go func() {
		_, _ = io.Copy(io.Discard, in)
		close(eof)
	}()
	select {
	case <-c:
	case <-eof:
	}
	return 0
}

// EndRouteHold ends a route's hold without retiring anything: a route whose
// env could not be built owns no use of the bridge it started. It ends the
// holder a route's handle names and waits, briefly, for its hold to lift:
// TERM, then poll until the holder is gone or the users lock it held reads
// free. A holder this process has reaped stays a zombie until Wait returns,
// so the lock, not the PID alone, must get the chance to say so. The PID is
// signalled only while the process table shows it as the core running
// `util route-hold` on users (holdsRoute): a number the system reused may be another
// session's process, and its hold — and the bridge that hold keeps alive —
// would end with it (#408).
func EndRouteHold(holder, users string) {
	n, err := strconv.Atoi(holder)
	if err != nil || n <= 0 || !holdsRoute(n, users) {
		return
	}
	syscall.Kill(n, syscall.SIGTERM)
	for range 200 { // ~2s
		if syscall.Kill(n, 0) != nil {
			return
		}
		if users != "" && !othersHold(users) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
