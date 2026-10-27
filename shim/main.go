package shim

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func envSeconds(name string, def float64) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return time.Duration(def * float64(time.Second))
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		die(fmt.Sprintf("gateway-shim: %s must be a number of seconds, got %q", name, v))
	}
	return time.Duration(f * float64(time.Second))
}

var pidRE = regexp.MustCompile(`^[0-9]+$`)

// Main is the shim's command line: `<port> <target> [flags]`, or the report,
// outcome and translator-user-agent subcommands. A private main supplies its
// Hooks; the stock binary passes none.
func Main(h Hooks) {
	argv := os.Args
	if len(argv) > 1 {
		switch argv[1] {
		case "report":
			rest := argv[2:]
			days := -1.0
			if len(rest) == 0 {
				days = 1
			} else if len(rest) == 2 && rest[0] == "--days" {
				if f, err := strconv.ParseFloat(strings.TrimSpace(rest[1]), 64); err == nil {
					days = f
				}
			}
			if !(days > 0) {
				die(fmt.Sprintf("usage: %s report [--days N]", argv[0]))
			}
			report(days)
			return
		case "outcome":
			if len(argv) != 4 {
				die(fmt.Sprintf("usage: %s outcome <task> accepted|rejected|escalated|parked", argv[0]))
			}
			recordOutcome(argv[2], argv[3])
			return
		case "translator-user-agent":
			if h.TranslatorUserAgent != nil {
				fmt.Println(h.TranslatorUserAgent())
			} else {
				fmt.Println()
			}
			return
		}
	}
	// Flags are position-free so the "<port> <target>" call form keeps working.
	var args []string
	flags := map[string]bool{}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "--") {
			flags[a] = true
		} else {
			args = append(args, a)
		}
	}
	var parentPID []string
	unknown := false
	for f := range flags {
		switch {
		case f == "--preserve-tool-ids", f == "--random-device-id", f == "--retry-transport":
		case strings.HasPrefix(f, "--parent-pid="):
			parentPID = append(parentPID, strings.TrimPrefix(f, "--parent-pid="))
		default:
			unknown = true
		}
	}
	if unknown || len(args) != 2 || len(parentPID) > 1 || (len(parentPID) == 1 && !pidRE.MatchString(parentPID[0])) {
		die(fmt.Sprintf("usage: %s <port> <target> [--preserve-tool-ids] [--random-device-id] "+
			"[--retry-transport] [--parent-pid=<pid>]", argv[0]))
	}
	port, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil {
		die(fmt.Sprintf("gateway-shim: invalid port %q", args[0]))
	}
	s := NewServer(args[1])
	s.Hooks = h
	s.Stall = envSeconds("CCMORPH_STALL_TIMEOUT", 300)
	s.RetryBase = envSeconds("CCMORPH_RETRY_BASE", 0.5)
	s.setClients()
	if spec := os.Getenv("CCMORPH_MODEL_ROUTE"); spec != "" {
		if s.Routes, err = newModelRoutes(spec, s.remote); err != nil {
			die("gateway-shim: " + err.Error())
		}
	}
	if raw := os.Getenv("CCMORPH_INBOUND"); raw != "" {
		in, err := parseInbound(raw)
		if err != nil {
			die("gateway-shim: CCMORPH_INBOUND: " + err.Error())
		}
		s.Inbound = in
	}
	s.PreserveIDs = flags["--preserve-tool-ids"]
	s.RandomDeviceID = flags["--random-device-id"]
	s.RetryTransport = flags["--retry-transport"]
	if s.RetryTransport {
		s.Breaker = NewBreaker()
	}
	// Loopback only; no reverse lookup of the name (it can stall startup on macOS).
	ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		die("gateway-shim: " + err.Error())
	}
	if len(parentPID) == 1 {
		// ccmorph-adapter's contract: die with the launcher even under SIGKILL,
		// which no in-process cleanup can cover. An orphan has nothing to flush.
		watched, _ := strconv.Atoi(parentPID[0])
		go func() {
			for {
				time.Sleep(2 * time.Second)
				if syscall.Kill(watched, 0) != nil {
					os.Exit(0)
				}
			}
		}()
	}
	fmt.Printf("gateway-shim :%d -> %s\n", ln.Addr().(*net.TCPAddr).Port, args[1])
	srv := &http.Server{Handler: s, ErrorLog: nil}
	die(srv.Serve(ln).Error())
}
