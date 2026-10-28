package adapter

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// logf writes the adapter's one stderr line format.
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ccmorph-adapter: %s\n", fmt.Sprintf(format, a...))
}

// Exit paths all go through exit so the cleanups (rendered state, restored
// config) run on every one of them, like the Python atexit handlers.
var (
	exitMu   sync.Mutex
	cleanups []func()
)

func atExit(fn func()) {
	exitMu.Lock()
	defer exitMu.Unlock()
	cleanups = append(cleanups, fn)
}

func exit(code int) {
	exitMu.Lock()
	fns := cleanups
	cleanups = nil
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
	os.Exit(code)
}

func die(format string, a ...any) {
	logf(format, a...)
	exit(1)
}

// abortAfter bounds selected-model classification; it runs serially so cleanup
// can unwind. The returned func disarms it.
func abortAfter(d time.Duration, message string) func() {
	t := time.AfterFunc(d, func() {
		logf("%s", message)
		exit(1)
	})
	return func() { t.Stop() }
}

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// loopbackListenerExists is true when something would answer a connection to
// this endpoint. A bind-table probe cannot ask that question (macOS lets a
// specific bind succeed right over a wildcard listener); connecting does.
func loopbackListenerExists(port int) bool {
	c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err == nil {
		c.Close()
		return true
	}
	// Refused means nothing listens; anything else (a timeout cannot really
	// happen on loopback) counts the port as taken -- fail safe for a credential
	// endpoint.
	return !strings.Contains(err.Error(), "refused")
}

// reservePort holds a fixed port from validation until the shim takes it over.
// The whole startup runs between validating --port and the shim's bind; held,
// the worst case is the sub-second gap between release and the shim's bind.
// SO_REUSEADDR so a restart right after a stop is not refused over TIME_WAIT,
// and a bound socket still blocks every other binder.
func reservePort(port int) (int, error) {
	if loopbackListenerExists(port) {
		return -1, fmt.Errorf("something is already serving on 127.0.0.1:%d", port)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// readInboundToken reads the inbound credential of a long-lived endpoint from a
// 0600 file. Everything is judged through the one O_NOFOLLOW fd, so there is no
// window between the check and the read. The token is a credential: every
// message names the path, never the content.
func readInboundToken(path string) string {
	refuse := func(why string) { die("inbound token file %s: %s", path, why) }
	// O_NONBLOCK so a FIFO cannot hold the open forever; a symlink dies with ELOOP
	// before any content is read.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		refuse(fmt.Sprintf("cannot be read (%s)", err.(syscall.Errno).Error()))
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		refuse(fmt.Sprintf("cannot be read (%s)", err.(syscall.Errno).Error()))
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		refuse("is not a regular file")
	}
	if st.Mode&0o777 != 0o600 {
		refuse(fmt.Sprintf("must have mode 0600 (has %04o)", st.Mode&0o777))
	}
	if int(st.Uid) != os.Getuid() {
		refuse("is not owned by you")
	}
	// Bounded: no real credential is pages long.
	var data []byte
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if n <= 0 || err != nil {
			break
		}
		data = append(data, buf[:n]...)
		if len(data) > 4096 {
			refuse("holds more than 4096 characters")
		}
	}
	if !utf8.Valid(data) {
		refuse("is not valid utf-8")
	}
	token := strings.TrimSpace(string(data))
	if utf8.RuneCountInString(token) < 32 {
		refuse("holds fewer than 32 characters")
	}
	// The token travels as an HTTP header value; a control character in it cannot
	// be sent, so it would surface much later as a readiness timeout.
	if controlChars.MatchString(token) {
		refuse("holds control characters")
	}
	return token
}

// proc is a child process with its exit observable from several goroutines.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func start(cmd *exec.Cmd) (*proc, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *proc) code() int { return p.cmd.ProcessState.ExitCode() }

// stopGrace is each process's own SIGTERM-to-SIGKILL budget.
var stopGrace = 10 * time.Second

// stopProcesses terminates every live process and gives each its OWN grace
// budget: a shared deadline let an early process that ignores SIGTERM consume
// the whole window, so a later one was SIGKILLed with zero grace mid-token-refresh.
// The waits overlap in wall-clock because all were already terminated.
func stopProcesses(procs ...*proc) {
	var live []*proc
	for _, p := range procs {
		if p != nil && !p.exited() {
			live = append(live, p)
			p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	var survivors []*proc
	for _, p := range live {
		select {
		case <-p.done:
		case <-time.After(stopGrace):
			survivors = append(survivors, p)
		}
	}
	for _, p := range survivors {
		p.cmd.Process.Kill()
	}
	for _, p := range survivors {
		<-p.done
	}
}

// tether starts a separate tiny process that outlives a SIGKILLed adapter: the
// adapter's own handlers cannot run then, and macOS has no parent-death signal,
// so its children were reparented to launchd and served an empty port forever
// (40 translator+shim pairs found leaked on 2026-10-02). Once the adapter is
// gone the tether stops the child with the same TERM-then-KILL grace and removes
// the state directory holding the credential.
func tether(exe string, child *proc, state string) {
	cmd := exec.Command(exe, "tether", strconv.Itoa(os.Getpid()), strconv.Itoa(child.cmd.Process.Pid), state)
	// Its own session, so the hangup that ends a tmux session cannot take the
	// tether down before the adapter's SIGHUP handler has stopped the child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// Exit runs the registered cleanups, then exits with code.
func Exit(code int) { exit(code) }

// Tether is the `ccmorph-core tether <adapter-pid> <child-pid> <state-dir>` body.
func Tether(args []string) {
	if len(args) != 3 {
		os.Exit(2)
	}
	adapter, _ := strconv.Atoi(args[0])
	child, _ := strconv.Atoi(args[1])
	state := args[2]
	alive := func() bool { return syscall.Kill(child, 0) == nil }
	for os.Getppid() == adapter && alive() {
		time.Sleep(2 * time.Second)
	}
	if alive() {
		syscall.Kill(child, syscall.SIGTERM)
		deadline := time.Now().Add(10 * time.Second)
		for alive() && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if alive() {
			syscall.Kill(child, syscall.SIGKILL)
		}
	}
	if state != "" && os.Getppid() != adapter {
		os.RemoveAll(state)
	}
}
