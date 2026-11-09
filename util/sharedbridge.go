package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// ours is whether pid still runs the bridge whose config lives in tmp, read
// from the process table, never a reused PID.
func ours(pid int, tmp string) bool {
	if pid <= 0 || tmp == "" {
		return false
	}
	return strings.Contains(processTable()[pid].args, filepath.Join(tmp, "config.yaml"))
}

// answers is whether the recorded bridge serves. One failed probe proves
// nothing while the bridge still runs its config and another session holds it:
// joining a slow bridge keeps one refresher, while killing it and starting a
// second signs the profile out (#294). Without a holder alive, a bounded retry
// is what says dead.
func answers(pid, port, key, tmp string, held bool) bool {
	n, _ := strconv.Atoi(pid)
	for i := 0; i < bridgeProbeTries; i++ {
		if probeBridge(port, key) {
			return true
		}
		if !ours(n, tmp) {
			return false // dead or a reused PID: nothing to wait for
		}
		if held {
			return true
		}
	}
	return false
}

// stopIfOurs stops a recorded bridge only while that PID still runs the
// cliproxyapi whose config lives in tmp, never a reused PID.
func stopIfOurs(pid, tmp string) {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 || tmp == "" {
		return
	}
	if ours(n, tmp) {
		BridgeStop(n, tmp, "")
	} else if _, err := os.Stat(tmp); err == nil && strings.Contains(filepath.Base(tmp), "ccmorph-bridge-") {
		os.RemoveAll(tmp)
	}
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
		if !ours(pid, b.TmpDir) {
			pid = 0 // a bridge that died left its PID to whatever the system started next
		}
		BridgeStop(pid, b.TmpDir, authDir)
		os.Remove(b.state)
	}
	b.users.Close()
	b.users = nil
}
