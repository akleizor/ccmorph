//go:build !darwin

package util

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// sysctlProcessTable is the kernel's own process list on a platform without
// sysctl: /proc read without ps, which a busybox Linux cannot be asked for a
// table at all (#1051). False where /proc cannot speak, so processTable keeps
// its ps fallback.
func sysctlProcessTable() (map[int]psRow, bool) { return procfsProcessTable() }

// procStat is /proc/<pid>/stat split at its comm field: comm may hold spaces
// and parentheses, so the fields are the ones past the last ')'. False when the
// file cannot be read or holds no comm.
func procStat(pid int) (comm string, rest []string, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", nil, false
	}
	l := bytes.IndexByte(b, '(')
	i := bytes.LastIndexByte(b, ')')
	if l < 0 || i <= l {
		return "", nil, false
	}
	return string(b[l+1 : i]), strings.Fields(string(b[i+1:])), true
}

// procStartToken is the kernel's own mark of when pid started: this boot's id
// and the process's start in clock ticks since boot, field 22 of
// /proc/<pid>/stat. Ticks since boot are not the wall clock, so no step moves
// them, and the boot id keeps a later boot's process apart. False where there
// is no /proc, or the pid is gone.
func procStartToken(pid int) (string, bool) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	_, f, ok := procStat(pid)
	if !ok || len(f) < 20 {
		return "", false
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], true
}

// userHZ is the unit of /proc/<pid>/stat's start field: the kernel's USER_HZ,
// 100 on every Linux this ships (linux/amd64 and linux/arm64). A kernel with a
// larger rate would make the derived start earlier than the true one, which
// keeps the sweep rather than reaping on it.
const userHZ = 100

// procfsProcessTable is processTable from /proc alone: every visible pid's
// parent, comm, argv and start, with no ps to fail on. A start is field 22 of
// /proc/<pid>/stat, clock ticks since boot, anchored by /proc/uptime read at
// the same moment; neither depends on ps existing or on lstart's locale. False
// when /proc cannot speak — it is not mounted, its uptime cannot be read, or
// this process does not read under its real parent — so the caller keeps the
// ps fallback.
func procfsProcessTable() (map[int]psRow, bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	up, ok := procUptime()
	if !ok {
		return nil, false
	}
	now := time.Now()
	rows := make(map[int]psRow, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if r, ok := procRow(pid, now, up); ok {
			rows[pid] = r
		}
	}
	self, ok := rows[os.Getpid()]
	if !ok || self.ppid != os.Getppid() {
		return nil, false
	}
	// A start this reader cannot derive stays the cannot-tell zero, which the
	// sweep keeps; but a self that reads as never started or as starting later
	// than now means field 22 is not the field this reader thinks. Then every
	// time is dropped to the cannot-tell zero rather than a wrong one reaped on.
	if self.start.IsZero() || self.start.After(now) {
		for pid, r := range rows {
			r.start = time.Time{}
			rows[pid] = r
		}
	}
	return rows, true
}

// procUptime is /proc/uptime's first field: seconds since boot, the clock the
// kernel measures a process's start ticks against. False when it cannot be
// read or does not parse.
func procUptime() (float64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	up, err := strconv.ParseFloat(f[0], 64)
	if err != nil || up < 0 {
		return 0, false
	}
	return up, true
}

// procRow is pid's row from /proc: parent and comm from /proc/<pid>/stat, argv
// from /proc/<pid>/cmdline joined the way ps's command column reads it, and the
// start /proc/uptime anchors field 22 to. False when the process left between
// the directory read and this one.
func procRow(pid int, now time.Time, uptime float64) (psRow, bool) {
	comm, f, ok := procStat(pid)
	if !ok || len(f) < 2 {
		return psRow{}, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return psRow{}, false
	}
	r := psRow{ppid: ppid, comm: comm, args: procCmdline(pid)}
	if len(f) < 20 {
		return r, true
	}
	ticks, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return r, true
	}
	// Clock ticks since boot against /proc/uptime's seconds since boot, read
	// around the same instant: their difference is the process's age, and the
	// wall clock minus that age is when it started.
	if age := uptime - float64(ticks)/userHZ; age >= 0 {
		r.start = now.Add(-time.Duration(age * float64(time.Second)))
	}
	return r, true
}

// procCmdline is pid's argv from /proc/<pid>/cmdline, arguments joined with a
// space the way ps's command column reads them; "" when the command line is
// empty (a kernel thread) or cannot be read (the pid left).
func procCmdline(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	return strings.Join(strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), " ")
}

// imageOf is pid's executable path and argv, from /proc/<pid>/exe and
// /proc/<pid>/cmdline; false when the command line cannot be read, an
// unreadable executable link leaving the path empty.
func imageOf(pid int) (exe string, argv []string, ok bool) {
	argv, ok = argvOf(pid)
	if !ok {
		return "", nil, false
	}
	if p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		exe = p
	}
	return exe, argv, true
}

// A just-exec'd process reaches /proc without its command line for a moment:
// the kernel closes the exec-status pipe Start waits on before it publishes
// the new argv, so the parent that started the pid can read the file still
// empty. Read it again over that moment, briefly, so a launch does not read a
// bridge it just started as dead and start a second.
const (
	argvOfReads = 20
	argvOfPause = time.Millisecond
)

// argvOf is pid's argv with its argument boundaries, from /proc/<pid>/cmdline,
// whose arguments are NUL-ended; false where there is no /proc, the pid is gone
// or the command line is empty.
func argvOf(pid int) ([]string, bool) {
	path := "/proc/" + strconv.Itoa(pid) + "/cmdline"
	for i := 0; i < argvOfReads; i++ {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		if len(b) > 0 {
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), true
		}
		if i == argvOfReads-1 || zombie(pid) {
			break
		}
		time.Sleep(argvOfPause)
	}
	return nil, false
}

// zombie is whether pid has exited and waits for its parent to reap it: its
// command line is empty and will not come back, so there is nothing to wait for.
func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}
