package agentproxy

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// EdgeMain is `ccmorph-core edge-proxy`: the Remote Control edge's proxy,
// started by `ccmorph-rc <route> --edge` (or by a bridge that brings up its
// own). Its configuration arrives through the environment, never argv; the
// route key itself arrives as a 0600 file that is deleted on read, so it is in
// neither the process table nor any environment:
//
//	RC_UPSTREAM        route endpoint (the adapter's loopback URL or the profile's base_url)
//	RC_KEY_FILE        route credential file, removed once read
//	RC_PARENT_PID      pid to follow; the proxy shuts down when it disappears
//	RC_PARENT_STARTED  that process's start (ccmorph-rc's proc_started): which edge a handoff is for
//	RC_LOG             optional path; one line per request, method and path only
//	RC_TIERS           optional JSON {alias: model}: tiers the config pinned (rc-hosts)
//	RC_INFERENCE       optional path of the switch file; the state is read beside it (edge.go)
//
// The flags: --listen <host:port> and --ca-dir <dir> (the CA kept there, made
// on first use), and --legacy-ca <pem> (the combined key and certificate of the
// proxy this one replaces, imported once so sessions that trust it keep trusting).
func EdgeMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("edge-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "address to serve on")
	caDir := fs.String("ca-dir", "", "directory the proxy's CA is kept in")
	legacy := fs.String("legacy-ca", "", "combined PEM of the CA to adopt when none is kept yet")
	if fs.Parse(args) != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "edge-proxy: "+format+"\n", a...)
		return 1
	}
	upstream, err := url.Parse(os.Getenv("RC_UPSTREAM"))
	if err != nil || upstream.Host == "" {
		return fail("RC_UPSTREAM is not a URL")
	}
	keyFile := os.Getenv("RC_KEY_FILE")
	os.Unsetenv("RC_KEY_FILE")
	key, err := os.ReadFile(keyFile)
	if keyFile == "" || err != nil {
		return fail("no route key file")
	}
	os.Remove(keyFile) // the key lives in memory only from here
	if *caDir == "" {
		return fail("--ca-dir is required")
	}
	parent, _ := strconv.Atoi(os.Getenv("RC_PARENT_PID"))
	var tiers map[string]string
	if t := os.Getenv("RC_TIERS"); t != "" {
		if tiers = stringMap([]byte(t)); tiers == nil {
			return fail("RC_TIERS is not a JSON object")
		}
	}
	e := NewEdge(&Edge{
		Upstream: upstream, Key: strings.TrimSpace(string(key)), Tiers: tiers,
		Inference: os.Getenv("RC_INFERENCE"), Parent: parent, ParentStarted: os.Getenv("RC_PARENT_STARTED"),
	})
	if path := os.Getenv("RC_LOG"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		e.Log = f
	}
	if err := os.MkdirAll(*caDir, 0o700); err != nil {
		return fail("%v", err)
	}
	ca, err := LoadEdgeCA(filepath.Clean(*caDir), *legacy)
	if err != nil {
		return fail("CA: %v", err)
	}
	p, err := Start(ca, Options{Listen: *listen, Edge: e, Dir: e.AgentsDir(), Logf: func(f string, a ...any) {
		e.logf(f, a...)
		fmt.Fprintf(stderr, f+"\n", a...)
	}})
	if err != nil {
		return fail("%v", err)
	}
	done := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	e.Run(done, func() { sig <- syscall.SIGTERM })
	<-sig
	close(done)
	p.Close()
	e.ForgetHeld()
	return 0
}
