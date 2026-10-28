// Package adapter puts a dialect translator in front of one ccmorph profile's
// endpoint: the Go port of libexec/ccmorph-adapter.
//
// Claude Code speaks exactly one request dialect, Anthropic /v1/messages. A
// gateway fronting several vendors often does not: models may answer only the
// OpenAI /responses dialect, or only /v1/chat/completions, and some reject
// parameters Claude Code always sends. cliproxyapi is the translator. It runs one
// instance per profile because its config has no way to reference a credential
// (a shared instance would let any profile spend any other profile's key), with
// its config rendered at launch and removed on exit. Which dialect a model serves
// is probed, never written down, and the verdicts are cached per endpoint at
// <state>/profiles/.adapter-cache/<sha256(base_url)[:16]>.json; delete that file
// to force re-probing after a gateway changes.
//
// Contract with ccmorph (unchanged from the Python adapter):
//
//	ccmorph-core adapter <profile> [flags]
//
// prints exactly one line to stdout when ready ("PORT=<n>", preceded by
// "TOKEN=<t>" only for a minted --inbound-token), after its own health check
// passes, then stays in the foreground supervising the translator. Anything else
// goes to stderr. It exits non-zero without printing PORT if it cannot serve. It
// terminates the translator and removes the rendered config when signalled or
// when --parent-pid disappears.
package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/shim"
)

// Timings, overridable for tests (the Python tests rewrote the constants in a
// copy of the script).
var (
	readyTimeout  = envSeconds("CCMORPH_ADAPTER_READY_TIMEOUT", 90)
	selectTimeout = envSeconds("CCMORPH_ADAPTER_SELECT_DEADLINE", 30)
	parentPoll    = 2 * time.Second
)

func envSeconds(name string, def int) time.Duration {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return time.Duration(def) * time.Second
}

// Seams for the unit tests.
var (
	profileEnvFn     = profileEnv
	classifyFn       = classify
	frontDirectlyFn  = frontDirectly
	servingFn        = serving
	freePortFn       = freePort
	detachStdin      = true
	profileDirs      = defaultProfileDirs()
	libDir           = defaultLibDir()
	translatorBinary = findTranslator
	spawnShim        = start
	cloakNeverFn     = func() bool { return cloakNeverFor(translatorBinary()) }
)

func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return exe
}

func home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func defaultProfileDirs() string {
	root := os.Getenv("CCMORPH_STATE_DIR")
	if root == "" {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home(), ".config")
		}
		root = filepath.Join(cfg, "ccmorph")
	}
	return filepath.Join(root, "profiles")
}

func defaultLibDir() string {
	if d := os.Getenv("CCMORPH_LIB_DIR"); d != "" {
		return d
	}
	exe := selfPath()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib")
}

func executable(p string) bool { return syscall.Access(p, 1) == nil }

// findTranslator discovers cliproxyapi from PATH first because a hardcoded path
// is only correct on one machine shape: /opt/homebrew is Apple-Silicon Homebrew,
// while Intel Macs and Linux use other locations. $HOME is honored, not the
// password database, so a per-user install is found where the caller says.
func findTranslator() string {
	if p, err := exec.LookPath("cliproxyapi"); err == nil {
		return p
	}
	for _, c := range []string{"/opt/homebrew/opt/cliproxyapi/bin/cliproxyapi",
		"/usr/local/opt/cliproxyapi/bin/cliproxyapi",
		"/usr/local/bin/cliproxyapi",
		"/usr/bin/cliproxyapi",
		filepath.Join(home(), ".local/bin/cliproxyapi")} {
		if executable(c) {
			return c
		}
	}
	// Nothing found: the conventional path, so the not-executable check produces
	// one clear, actionable error.
	return "/opt/homebrew/opt/cliproxyapi/bin/cliproxyapi"
}

// profileEnv returns the endpoint and credential, from the profile and nowhere
// else. The profile name is a positional argument, never interpolated into the
// script: it reaches this process from a caller's command line and `zsh -c`
// would otherwise execute anything it contains.
var runCommand = func(timeout time.Duration, name string, args ...string) (string, string, int) {
	cmd := exec.Command(name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if cmd.Start() != nil {
		return "", "", -1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		return stdout.String(), "timed out", -1
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

func ccmorphScript() string { return filepath.Join(libDir, "ccmorph.sh") }

func profileEnv(profile string) map[string]string {
	out, errText, code := runCommand(90*time.Second, "zsh", "-c", `source "$1" && ccmorph _env "$2"`,
		"ccmorph-adapter", ccmorphScript(), profile)
	if code != 0 {
		die("ccmorph _env %s failed: %s", profile, strings.TrimSpace(errText))
	}
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "="); ok {
			env[k] = v
		}
	}
	return env
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func kvs(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func environWith(extra map[string]string) []string { return append(os.Environ(), kvs(extra)...) }

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		die("cannot encode route: %v", err)
	}
	return string(raw)
}

type cliArgs struct {
	profile        string
	parentPID      int
	printConfig    bool
	auto           bool
	model          string
	inboundToken   bool
	tokenFile      *string
	ensureConfig   string
	ensurePort     int
	ensureIdentity string
	port           *int

	// resolved during startup
	token    string
	reserved int // fd of the held --port, or -1
}

const usage = "usage: ccmorph-adapter [-h] [--parent-pid PARENT_PID] [--print-config] [--auto]\n" +
	"                       [--model MODEL] [--inbound-token | --inbound-token-file FILE]\n" +
	"                       [--port PORT] profile\n"

// parseArgs is argparse for the adapter's flags: options anywhere, --opt=value
// or --opt value, one positional, exit 2 on misuse.
func parseArgs(argv []string) *cliArgs {
	fail := func(msg string) {
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintf(os.Stderr, "ccmorph-adapter: error: %s\n", msg)
		os.Exit(2)
	}
	a := &cliArgs{reserved: -1}
	var positional []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			if arg == "-h" {
				fmt.Print(usage)
				os.Exit(0)
			}
			positional = append(positional, arg)
			continue
		}
		name, val, hasVal := strings.Cut(arg, "=")
		value := func() string {
			if hasVal {
				return val
			}
			if i+1 >= len(argv) {
				fail(fmt.Sprintf("argument %s: expected one argument", name))
			}
			i++
			return argv[i]
		}
		number := func() int {
			n, err := strconv.Atoi(value())
			if err != nil {
				fail(fmt.Sprintf("argument %s: invalid int value", name))
			}
			return n
		}
		switch name {
		case "--parent-pid":
			a.parentPID = number()
		case "--print-config":
			a.printConfig = true
		case "--auto":
			a.auto = true
		case "--model":
			a.model = value()
		case "--inbound-token":
			a.inboundToken = true
		case "--inbound-token-file":
			v := value()
			a.tokenFile = &v
		case "--ensure-config":
			a.ensureConfig = value()
		case "--ensure-port":
			a.ensurePort = number()
		case "--ensure-identity":
			a.ensureIdentity = value()
		case "--port":
			n := number()
			a.port = &n
		case "--help":
			fmt.Print(usage)
			os.Exit(0)
		default:
			fail("unrecognized arguments: " + arg)
		}
	}
	if len(positional) != 1 {
		if len(positional) == 0 {
			fail("the following arguments are required: profile")
		}
		fail("unrecognized arguments: " + strings.Join(positional[1:], " "))
	}
	if a.inboundToken && a.tokenFile != nil {
		fail("argument --inbound-token-file: not allowed with argument --inbound-token")
	}
	a.profile = positional[0]
	return a
}

func (a *cliArgs) releasePort() {
	if a.reserved >= 0 {
		syscall.Close(a.reserved)
		a.reserved = -1
	}
}

func (a *cliArgs) checkParent(shutdown func(int)) {
	if a.parentPID != 0 && syscall.Kill(a.parentPID, 0) != nil {
		logf("parent gone, shutting down")
		shutdown(0)
	}
}

// watchSignals runs shutdown(0) on TERM, INT or HUP. HUP matters as much as
// TERM: closing a terminal or killing a tmux session hangs up the process
// group, and without this the rendered credential is left on disk.
func watchSignals(shutdown func(int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-ch; shutdown(0) }()
}

// frontDirectly serves an Anthropic-native gateway through the gateway-shim
// alone: no translator and no rendered credential, since the shim relays the
// client's own key. Same contract as the translator path.
func frontDirectly(a *cliArgs, upstream, key, routeIdentity string, usable []Row,
	extras map[string]string, privacyArgs []string, modelMap map[string]any) int {
	port := 0
	if a.port != nil {
		port = *a.port
	} else {
		var err error
		if port, err = freePortFn(); err != nil {
			die("no free port: %v", err)
		}
	}
	// No translator here, so the shim owns transport retries; behind cliproxyapi
	// its request-retry does, and a second layer would multiply attempts.
	hooksBin := strings.TrimSpace(extras["CCMORPH_SHIM_HOOKS"])
	o := shim.Options{Port: port, Target: upstream, PreserveIDs: true, RetryTransport: true,
		RandomDeviceID: contains(privacyArgs, "--random-device-id")}
	models := make([]string, len(usable))
	for i, r := range usable {
		models[i] = r.Model
	}
	if modelMap == nil {
		modelMap = map[string]any{}
	}
	env := map[string]string{"CCMORPH_MODEL_ROUTE": mustJSON(map[string]any{
		"profile": a.profile, "identity": routeIdentity, "upstream": upstream,
		"models_url": apiURL(upstream, "/v1/models"), "models": models,
		"model_map": modelMap, "timeout": int(selectTimeout.Seconds()),
		"key_sha256": hashHex(key)})}
	// Profile request quirks ride as env for the shim's hooks module, never as
	// new shim flags.
	for k, v := range extras {
		env[k] = v
	}
	if a.token != "" {
		env["CCMORPH_INBOUND"] = mustJSON(map[string]any{"sha256": hashHex(a.token), "key": key})
	}
	a.releasePort()
	shimProc, err := startShim(hooksBin, o, env, nil, os.Stderr)
	if err != nil {
		die("cannot start the gateway shim: %v", err)
	}

	shutdown := func(status int) {
		stopProcesses(shimProc)
		exit(status)
	}
	watchSignals(shutdown)

	deadline := time.Now().Add(readyTimeout)
	cred := a.token
	if cred == "" {
		cred = key
	}
	for {
		a.checkParent(shutdown)
		// The child is checked before the probe, never after: the probe carries
		// the inbound credential, and it must not be sent to a listener that may
		// not be our own shim.
		if shimProc.exited() || !time.Now().Before(deadline) {
			logf("gateway-shim did not serve on :%d", port)
			shutdown(1)
		}
		if servingFn(port, cred) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	hooks := ""
	if extras["CCMORPH_SHIM_HOOKS"] != "" {
		hooks = "; profile request hooks"
	}
	logf("gateway-shim interposed (Anthropic model ids; readable gateway errors%s)", hooks)
	// Only a minted --inbound-token is printed; a file token is a long-lived
	// credential whose stdout may be redirected to a file.
	if a.inboundToken {
		fmt.Printf("TOKEN=%s\n", a.token)
	}
	fmt.Printf("PORT=%d\n", port)
	logf("serving profile %s on :%d -> %s (no translator)", a.profile, port, upstream)
	for {
		if shimProc.exited() {
			logf("gateway-shim exited %d", shimProc.code())
			shutdown(1)
		}
		a.checkParent(shutdown)
		time.Sleep(parentPoll)
	}
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Main runs the adapter and returns its exit status.
func Main(argv []string) int {
	a := parseArgs(argv)

	// The long-lived endpoint form (--port with --inbound-token-file, for a
	// non-Claude client such as Hermes) validates everything up front, before the
	// stdin detach, the profile lookup or any serving: a launch caller aborts on
	// a dead port, and a service caller must never start half-configured. The
	// credential is judged before the port: its checks are purely local.
	a.reserved = -1
	if a.tokenFile != nil {
		a.token = readInboundToken(*a.tokenFile)
	} else if a.inboundToken {
		a.token = newToken()
	}
	if a.port != nil {
		if *a.port < 1024 || *a.port > 65535 {
			die("--port %d: must be 1024-65535", *a.port)
		}
		fd, err := reservePort(*a.port)
		if err != nil {
			die("--port %d: is in use", *a.port)
		}
		a.reserved = fd
	}

	// ccmorph's own [1m] marker (a context-window variant, not a distinct model id
	// on the gateway) is the shell layer's concern; strip only that exact suffix.
	required := strings.TrimSuffix(strings.TrimSpace(a.model), "[1m]")

	// Detach stdin from whatever the launcher handed us: it is inherited straight
	// from Claude Code's tty, and a foreground process that merely holds the tty
	// open races Claude Code's raw-mode reader for every keystroke (observed
	// 2026-08-06). fd 0 on /dev/null removes this process, and by inheritance the
	// translator child, from the tty entirely.
	if detachStdin {
		if fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0); err == nil {
			dup2(fd, 0)
			if fd > 0 {
				syscall.Close(fd)
			}
		}
	}

	env := profileEnvFn(a.profile)
	modelMap := map[string]any{}
	if raw := env["CCMORPH_MODEL_MAP"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &modelMap); err != nil {
			die("profile %s: CCMORPH_MODEL_MAP is not a JSON object: %v", a.profile, err)
		}
	}
	// Preparation keys the map by the wire model id, like the shim's request-side
	// lookup: never by the spelling the caller used.
	if a.model != "" && a.ensureConfig == "" {
		if mapped, ok := modelMap[required]; ok {
			required, _ = mapped.(string)
		}
		required = strings.TrimSuffix(required, "[1m]")
	}
	key := env["ANTHROPIC_API_KEY"]
	if key == "" {
		die("profile %s has no key", a.profile)
	}
	upstream := strings.TrimRight(env["ANTHROPIC_BASE_URL"], "/")
	if upstream == "" {
		die("profile %s has no base_url to front", a.profile)
	}
	upstreamProxy := strings.TrimSpace(env["CCMORPH_UPSTREAM_PROXY"])
	// Profile request quirks ride as CCMORPH_* env to the shim, not as shim flags.
	// CCMORPH_SHIM_HOOKS names a shim binary built with the profile's own hooks,
	// run in place of the stock one; validated before anything is spawned, so a
	// stale profile fails loudly at launch instead of inside the shim.
	extras := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "CCMORPH_") {
			extras[k] = v
		}
	}
	hooksPath := strings.TrimSpace(extras["CCMORPH_SHIM_HOOKS"])
	if hooksPath != "" {
		if st, err := os.Stat(hooksPath); err != nil || !st.Mode().IsRegular() || !executable(hooksPath) {
			die("profile %s points CCMORPH_SHIM_HOOKS at a missing or non-executable file: %s", a.profile, hooksPath)
		}
	}
	shimBinary := hooksPath // "": the stock shim, served inside this process

	// Dialect classification is a property of the ENDPOINT, not the profile, so
	// the cache is keyed by base_url in a shared dir: a second profile on a gateway
	// another already classified starts instantly. The stored endpoint is still
	// checked on load, so a hash collision or a moved URL is never mis-served.
	routeIdentity := hashHex("[" + q(upstream) + ", " + q(key) + "]")
	cacheFile := filepath.Join(profileDirs, ".adapter-cache", hashHex(upstream)[:16]+".json")

	if a.ensureConfig != "" {
		return ensureConfig(a, required, upstream, key, routeIdentity, cacheFile, upstreamProxy, shimBinary, extras)
	}

	// A selected launch is bounded end to end: it must fail fast rather than sit
	// through a lock wait plus probes on top of Claude Code's own launch timeout.
	// No --model keeps the old, unbounded diagnostic sweep.
	stop := func() {}
	if required != "" {
		stop = abortAfter(selectTimeout, fmt.Sprintf("classifying %s on %s exceeded %ds",
			shim.PyRepr(required), upstream, int(selectTimeout.Seconds())))
	}
	rows, err := classifyFn(upstream, key, cacheFile, required, a.auto)
	stop()
	if err != nil {
		die("%v", err)
	}
	return serve(a, rows, required, upstream, key, routeIdentity, upstreamProxy, shimBinary, extras, env, modelMap)
}

func ensureConfig(a *cliArgs, required, upstream, key, routeIdentity, cacheFile, upstreamProxy, shimBinary string,
	extras map[string]string) int {
	if a.ensureIdentity != routeIdentity {
		die("profile endpoint or credentials changed; resume the session to use the new route")
	}
	st, err := os.Lstat(a.ensureConfig)
	if required == "" || a.ensurePort == 0 || err != nil || !st.Mode().IsRegular() {
		die("invalid live model routing request")
	}
	conf := a.ensureConfig
	stop := abortAfter(selectTimeout, "selected model routing timed out")
	defer stop()
	advertised, err := inventoryFn(upstream, key, nil)
	if err != nil {
		die("%v", err)
	}
	if !contains(advertised, required) {
		die("model %s is not advertised by this gateway", shim.PyRepr(required))
	}
	rows, err := classifyFn(upstream, key, cacheFile, required, true)
	if err != nil {
		die("%v", err)
	}
	previous, err := os.ReadFile(conf)
	if err != nil {
		die("%v", err)
	}
	rendered := render(rows, key, a.ensurePort, upstream, filepath.Join(filepath.Dir(conf), "auth"),
		upstreamProxy, shimBinary, environWith(extras), cloakNeverFn())
	replace := func(data []byte) error {
		tmp, err := os.CreateTemp(filepath.Dir(conf), ".route-")
		if err != nil {
			return err
		}
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), conf)
		}
		if err != nil {
			os.Remove(tmp.Name())
		}
		return err
	}
	if err := replace([]byte(rendered)); err != nil {
		die("%v", err)
	}
	// Any failure or timeout from here restores the previous config.
	restore := func() { replace(previous) }
	atExit(restore)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ids, err := inventoryFn("http://127.0.0.1:"+strconv.Itoa(a.ensurePort), key, loopback)
		if err != nil {
			die("%v", err)
		}
		if contains(ids, required) {
			break
		}
		if !time.Now().Before(deadline) {
			die("translator has not activated the selected model")
		}
		time.Sleep(200 * time.Millisecond)
		now := time.Now()
		os.Chtimes(conf, now, now) // Watcher startup can lag HTTP readiness.
	}
	exitMu.Lock()
	cleanups = cleanups[:len(cleanups)-1] // success: keep the new config
	exitMu.Unlock()
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func serve(a *cliArgs, rows []Row, required, upstream, key, routeIdentity, upstreamProxy, shimBinary string,
	extras map[string]string, env map[string]string, modelMap map[string]any) int {
	var usable []Row
	for _, r := range rows {
		if r.Dialect != "unusable" {
			usable = append(usable, r)
		}
	}
	if len(usable) == 0 {
		if a.auto && required == "" {
			fmt.Printf("EXIT=no declared or cached model protocol; set a model with ccmorph set %s model <model-id> and retry (the compatibility check may incur a small charge)\n", a.profile)
			return 1
		}
		die("no usable models on this endpoint; refusing to start")
	}

	// Native is a hard fast path: do not require cliproxyapi, create temp state,
	// or probe workaround behavior when every usable row is plain Anthropic.
	native := true
	recyclesIDs := false
	for _, r := range usable {
		native = native && r.Dialect == "anthropic" && !r.capped() && !r.Nothink && !r.IDRecycle
		recyclesIDs = recyclesIDs || r.IDRecycle
	}
	// Identifier handling is a per-profile opt-in; without it the shim forwards
	// the client's metadata and headers as sent.
	var privacyArgs []string
	if env["CCMORPH_RANDOM_DEVICE_ID"] == "1" {
		privacyArgs = []string{"--random-device-id"}
	}
	if stall := strings.TrimSpace(env["CCMORPH_STALL_TIMEOUT"]); stall != "" {
		os.Setenv("CCMORPH_STALL_TIMEOUT", stall)
	}
	// Every route goes through the gateway edge (gateway-shim): Anthropic model
	// ids in and out, a moving catalogue, readable errors, stall deadlines. A
	// gateway that already speaks Anthropic needs no translator.
	if a.auto && native && !a.printConfig {
		return frontDirectlyFn(a, upstream, key, routeIdentity, usable, extras, privacyArgs, modelMap)
	}

	tmpRoot := os.Getenv("TMPDIR")
	if tmpRoot == "" {
		tmpRoot = "/tmp"
	}
	state, err := os.MkdirTemp(tmpRoot, fmt.Sprintf("ccmorph-adapter-%d-", os.Getuid()))
	if err != nil {
		die("cannot create state directory: %v", err)
	}
	os.Chmod(state, 0o700)
	atExit(func() { os.RemoveAll(state) })

	port, err := freePortFn()
	if err != nil {
		die("no free port: %v", err)
	}
	conf := filepath.Join(state, "config.yaml")
	authdir := filepath.Join(state, "auth")
	if err := os.Mkdir(authdir, 0o700); err != nil {
		die("%v", err)
	}
	os.Remove(conf)

	syscall.Umask(0o077)
	text := render(rows, key, port, upstream, authdir, upstreamProxy, shimBinary, environWith(extras), cloakNeverFn())
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		die("%v", err)
	}
	os.Chmod(conf, 0o600)

	if a.printConfig {
		// Redact both spellings: the key is on disk as a YAML scalar, so one with a
		// quote or backslash is in its escaped form and a raw replace alone would
		// walk straight past it.
		quoted := q(key)
		for _, spelling := range []string{quoted[1 : len(quoted)-1], key} {
			text = strings.ReplaceAll(text, spelling, "<redacted>")
		}
		if upstreamProxy != "" {
			text = strings.ReplaceAll(text, q(upstreamProxy), q(redactURL(upstreamProxy)))
		}
		os.Remove(conf)
		fmt.Println(text)
		return 0
	}

	translator := translatorBinary()
	if !executable(translator) {
		os.Remove(conf)
		// Name the command, not just the concept: this fires on a first run against
		// a copied profile, where the reader has never heard of the translator.
		die("translator not found or not executable: %s\nThis endpoint needs cliproxyapi for automatic protocol compatibility.\n  brew install cliproxyapi\nPut cliproxyapi on PATH and try again.", translator)
	}

	logPath := filepath.Join(state, "translator.log")
	logfile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		die("%v", err)
	}
	atExit(func() { logfile.Close() })
	// stdin is /dev/null deliberately: the translator is an HTTP server that never
	// reads stdin, and it must not inherit (and thus hold open) the controlling
	// terminal, which would steal keystrokes from Claude Code's TUI.
	cmd := exec.Command(translator, "-config", conf)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	child, err := start(cmd)
	if err != nil {
		die("cannot start translator %s: %v", translator, err)
	}
	tether(selfPath(), child, state)

	var shimProc atomic.Pointer[proc]
	// shutdown stops the children, removes the credential and exits with status.
	// Signal handlers exit 0 -- an orderly stop we were told to make. Failure
	// paths pass 1: anything short of "printed PORT and served" exits non-zero, so
	// the caller can abort the launch instead of pointing Claude Code at a dead
	// port.
	shutdown := func(status int) {
		stopProcesses(shimProc.Load(), child)
		os.Remove(conf)
		exit(status)
	}
	watchSignals(shutdown)

	waitReady := func(p *proc, port int, what, auth string) {
		if auth == "" {
			auth = key
		}
		deadline := time.Now().Add(readyTimeout)
		for time.Now().Before(deadline) {
			a.checkParent(shutdown)
			if p.exited() {
				logf("%s exited %d; see %s", what, p.code(), logPath)
				shutdown(1)
			}
			if servingFn(port, auth) {
				return
			}
			time.Sleep(400 * time.Millisecond)
		}
		// Never served: a failure. Exiting 0 here made a timed-out launch look
		// successful to ccmorph, which then ran Claude Code against a dead port.
		logf("%s did not serve on :%d within %ds; see %s", what, port, int(readyTimeout.Seconds()), logPath)
		shutdown(1)
	}
	waitReady(child, port, "translator", "")

	// The stable request shim prepares newly selected models before forwarding;
	// response-id recycling and the profile's request hooks are gated in the
	// shim's own env, not in flags.
	shimPort := 0
	if a.port != nil {
		shimPort = *a.port
	} else if shimPort, err = freePortFn(); err != nil {
		die("no free port: %v", err)
	}
	models := make([]string, len(usable))
	for i, r := range usable {
		models[i] = r.Model
	}
	shimEnv := map[string]string{"CCMORPH_MODEL_ROUTE": mustJSON(map[string]any{
		"adapter": selfPath(), "adapter_args": []string{"adapter"},
		"profile": a.profile, "config": conf, "port": port, "identity": routeIdentity, "upstream": upstream,
		"models": models, "model_map": modelMap, "timeout": int(selectTimeout.Seconds()),
		"key_sha256": hashHex(key)})}
	for k, v := range extras {
		shimEnv[k] = v
	}
	if a.token != "" {
		shimEnv["CCMORPH_INBOUND"] = mustJSON(map[string]any{"sha256": hashHex(a.token), "key": key})
	}
	a.releasePort()
	sp, err := startShim(shimBinary, shim.Options{Port: shimPort, Target: "http://127.0.0.1:" + strconv.Itoa(port),
		PreserveIDs: !recyclesIDs, RandomDeviceID: contains(privacyArgs, "--random-device-id")}, shimEnv, logfile, logfile)
	if err != nil {
		logf("cannot start the gateway shim: %v", err)
		shutdown(1)
	}
	shimProc.Store(sp)
	waitReady(sp, shimPort, "gateway-shim", a.token)
	why := []string{"prepare models selected mid-session"}
	if recyclesIDs {
		why = append(why, "upstream recycles tool_use ids")
	}
	if extras["CCMORPH_SHIM_HOOKS"] != "" {
		why = append(why, "profile request hooks")
	}
	logf("gateway-shim interposed (%s)", strings.Join(why, "; "))

	// The one contract line, only after the health check passed.
	if a.inboundToken {
		fmt.Printf("TOKEN=%s\n", a.token)
	}
	fmt.Printf("PORT=%d\n", shimPort)
	logf("serving %d model(s) for profile %s on :%d (translator :%d)", len(usable), a.profile, shimPort, port)

	// Supervise rather than exec, so an orphan can notice its parent is gone --
	// no trap fires when the wrapper is SIGKILLed.
	for {
		if child.exited() {
			logf("translator exited %d", child.code())
			shutdown(1)
		}
		if sp.exited() {
			logf("gateway-shim exited %d", sp.code())
			shutdown(1)
		}
		a.checkParent(shutdown)
		time.Sleep(parentPoll)
	}
}

// redactURL has the shape of ccmorph.sh's _ccm_redact_url: no userinfo, no query.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	if host == "" {
		return u.Scheme + ":" + u.Path
	}
	return u.Scheme + "://" + host + u.Path
}
