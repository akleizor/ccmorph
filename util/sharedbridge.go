package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

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

// AcquireBridge returns the profile's running bridge, or starts one with start.
// Files <root>/.<name>.bridge{,.lock,.users} hold its handle, the start/stop
// mutex and the users' shared locks.
func AcquireBridge(root, name string, start func() (pid, port, key, tmp string, ok bool)) (*SharedBridge, bool) {
	base := filepath.Join(root, "."+name+".bridge")
	mu, err := lockFile(base+".lock", syscall.LOCK_EX)
	if err != nil {
		return nil, false
	}
	defer mu.Close()
	users, err := lockFile(base+".users", syscall.LOCK_SH)
	if err != nil {
		return nil, false
	}
	b := &SharedBridge{mutex: base + ".lock", state: base, users: users}
	if raw, err := os.ReadFile(base); err == nil {
		if f := strings.Fields(string(raw)); len(f) == 4 {
			if listsModel(f[1], f[2]) {
				b.PID, b.Port, b.Key, b.TmpDir = f[0], f[1], f[2], f[3]
				return b, true
			}
			stopIfOurs(f[0], f[3]) // a stuck bridge would still refresh the token
		}
	}
	pid, port, key, tmp, ok := start()
	if !ok {
		users.Close()
		return nil, false
	}
	b.PID, b.Port, b.Key, b.TmpDir = pid, port, key, tmp
	if os.WriteFile(base, []byte(strings.Join([]string{pid, port, key, tmp}, " ")), 0o600) != nil {
		os.Remove(base) // unrecorded: the next launch must not trust an old handle
	}
	return b, true
}

// stopIfOurs stops a recorded bridge only while that PID still runs the
// cliproxyapi whose config lives in tmp, never a reused PID.
func stopIfOurs(pid, tmp string) {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 || tmp == "" {
		return
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", pid).Output()
	if err == nil && strings.Contains(string(out), filepath.Join(tmp, "config.yaml")) {
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
	if mu, err := lockFile(b.mutex, syscall.LOCK_EX); err == nil {
		defer mu.Close()
	}
	fd := int(b.users.Fd())
	syscall.Flock(fd, syscall.LOCK_UN)
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		pid, _ := strconv.Atoi(b.PID)
		BridgeStop(pid, b.TmpDir, authDir)
		os.Remove(b.state)
	}
	b.users.Close()
	b.users = nil
}
