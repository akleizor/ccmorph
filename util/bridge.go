package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// bridgeStart is `util bridge-start <profile> <cliproxyapi> <auth dir> <log>`: run
// a provider profile's cliproxyapi on a loopback port with a fresh inbound key and
// print PID=, PORT=, KEY= and TMPDIR= once it serves. The bridge is left running;
// bridge-stop retires it.
func bridgeStart(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 4 {
		return 2
	}
	profile, bin, authDir, logPath := args[0], args[1], args[2], args[3]
	fail := func(msg string) int { fmt.Fprintln(errw, "ccmorph: "+msg); return 1 }
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return 1
	}
	key := hex.EncodeToString(raw)
	tmp, err := os.MkdirTemp(os.Getenv("TMPDIR"), "ccmorph-bridge-"+strconv.Itoa(os.Getuid())+"-")
	if err != nil {
		return 1
	}
	_ = os.Chmod(tmp, 0o700)
	port, err := pickPort()
	if err != nil {
		os.RemoveAll(tmp)
		return 1
	}
	// The profile is pinned to one model: a silent quota-time substitution would make
	// every window ccmorph computed describe another model, and antigravity-credits
	// would spend paid credits. Keep it pinned and fail loud instead.
	conf := filepath.Join(tmp, "config.yaml")
	yaml := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys:
  - %q
debug: false
disable-cooling: true
commercial-mode: true
logging-to-file: false
switch-project: false
switch-preview-model: false
antigravity-credits: false
`, port, authDir, key)
	if os.WriteFile(conf, []byte(yaml), 0o600) != nil {
		os.RemoveAll(tmp)
		return 1
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		logf, _ = os.Open(os.DevNull)
	}
	cmd := exec.Command(bin, "-config", conf)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmp)
		return fail("bridge for '" + profile + "' exited before becoming ready")
	}
	logf.Close()
	died := make(chan struct{})
	go func() { _ = cmd.Wait(); close(died) }()
	stop := func() { bridgeStop([]string{strconv.Itoa(cmd.Process.Pid), tmp, authDir}, nil, out, errw) }

	// A bound port is not a ready router: it listens before its model catalogue
	// loads. Only a 200 from /v1/models, the client's own contract, is ready. No
	// proxy: an ambient HTTPS_PROXY answers for loopback while the bridge is fine.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-died:
			stop()
			return fail("bridge for '" + profile + "' exited before becoming ready")
		default:
		}
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Fprintf(out, "PID=%d\nPORT=%d\nKEY=%s\nTMPDIR=%s\n", cmd.Process.Pid, port, key, tmp)
				return 0
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	return fail(fmt.Sprintf("bridge for '%s' on port %d: /v1/models never returned 200 within 30s", profile, port))
}

// bridgeStop is `util bridge-stop <pid> <tmpdir> <auth dir>`: TERM, then KILL after
// 5s, then re-tighten the OAuth files cliproxyapi rewrote at its own umask (observed
// 0644) to the 0600 SECURITY.md promises, and remove the rendered config.
func bridgeStop(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 3 {
		return 2
	}
	if pid, err := strconv.Atoi(args[0]); err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if !waitGone(pid, 5*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(pid, 2*time.Second)
		}
	}
	if args[2] != "" {
		_ = filepath.Walk(args[2], func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() && filepath.Ext(p) == ".json" {
				_ = os.Chmod(p, 0o600)
			}
			return nil
		})
	}
	if args[1] != "" {
		_ = os.RemoveAll(args[1])
	}
	return 0
}

func waitGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if syscall.Kill(pid, 0) != nil {
			return true
		}
	}
	return false
}

func pickPort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
