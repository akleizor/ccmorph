// The util verbs a v0.4.6 launcher still running across an update execs on the
// on-disk core (#162). Their argv, env, stdout and exit status are 6860fb5's; each
// one adapts to the typed function the v0.4.7 cli calls directly.
package util

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// v046Verbs are merged into verbs: the names a v0.4.6 launcher sends by path.
var v046Verbs = map[string]verb{
	"config-store":        v046ConfigStore,
	"bridge-stop":         v046BridgeStop,
	"fetch-listing":       v046FetchListing,
	"project-picker":      v046ProjectPicker,
	"hazard-set":          v046HazardSet,
	"hazard-msg":          v046HazardMsg,
	"advise-json":         v046AdviseJSON,
	"withhold-fatal":      v046WithholdFatal,
	"launch-diagnostics":  v046LaunchDiagnostics,
	"listing-has-unsized": v046ListingHasUnsized,
	"unwire-statusline":   v046UnwireStatusline,
	"launch-window":       v046LaunchWindow,
}

// v046ConfigStore is 6860fb5's util config-store: <op> <conf> <settings> <key>
// <shared> [mapping...], the change and materialize argv in _CCM_* env.
func v046ConfigStore(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) < 5 {
		fmt.Fprintln(errw, "usage: util config-store <op> <conf> <settings> <key> <shared> [mapping keys...]")
		return 2
	}
	a := StoreArgs{
		Op: args[0], Conf: args[1], Settings: args[2], Key: args[3], Shared: args[4],
		Mapping: args[5:], Materialize: os.Getenv("_CCM_MATERIALIZE"),
		Change: StoreChange{
			Model: os.Getenv("_CCM_CONFIG_MODEL"), Effort: os.Getenv("_CCM_CONFIG_EFFORT"),
			Snapshot: os.Getenv("_CCM_CONFIG_SNAPSHOT"), Type: os.Getenv("_CCM_CONFIG_TYPE"),
			Endpoint: os.Getenv("_CCM_CONFIG_ENDPOINT"), Credential: os.Getenv("_CCM_CONFIG_CREDENTIAL"),
		},
	}
	s, rc := ConfigStore(a, errw)
	io.WriteString(out, s)
	return rc
}

// v046BridgeStop is 6860fb5's util bridge-stop: <pid> <tmpdir> <auth dir>.
func v046BridgeStop(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 3 {
		return 2
	}
	pid, _ := strconv.Atoi(args[0])
	if pid < 0 {
		pid = 0
	}
	BridgeStop(pid, args[1], args[2])
	return 0
}

// v046Lines is a v0.4.6 env list: its non-blank lines.
func v046Lines(env string) []string {
	var out []string
	for _, l := range strings.Split(env, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// v046FetchListing is 6860fb5's util fetch-listing: <base> <mode> [max-time], the
// key on stdin's first line.
func v046FetchListing(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 2 {
		return 2
	}
	base, mode, maxTime := args[0], args[1], "20"
	if len(args) > 2 {
		maxTime = args[2]
	}
	key, _ := bufio.NewReader(in).ReadString('\n')
	key = strings.TrimRight(key, "\r\n")
	switch mode {
	case "launch":
		secs, err := strconv.Atoi(maxTime)
		if err != nil {
			secs = 20
		}
		body, ok := LaunchListing(base, key, secs)
		if !ok {
			return 1
		}
		io.WriteString(out, body)
		return 0
	case "models":
		body, ok := ModelsListing(base, key, errw)
		if !ok {
			return 1
		}
		io.WriteString(out, body)
		return 0
	case "doctor":
		body, code := ProbeListing(base, key)
		fmt.Fprintf(out, "%s\n%s", body, code)
		return 0
	}
	return 2
}

// v046ProjectPicker is 6860fb5's util project-picker: <home> <conf> <own> <tag>
// <profile> <layer> <launch>, the listing in _CCM_MODELS_JSON.
func v046ProjectPicker(args []string, _ io.Reader, _, errw io.Writer) int {
	if len(args) != 7 {
		return 2
	}
	ok := ProjectPicker(PickerIn{
		Home: args[0], Conf: args[1], Own: args[2], Tag: args[3], Profile: args[4],
		Layer: args[5], Launch: args[6], ModelsJSON: os.Getenv("_CCM_MODELS_JSON"),
	}, errw)
	if !ok {
		return 1
	}
	return 0
}

// v046HazardSet is 6860fb5's util hazard-set: its operands all in the _CCM_ env.
func v046HazardSet(_ []string, _ io.Reader, out, errw io.Writer) int {
	rows, err := HazardSet(HazardIn{
		Profile: os.Getenv("_CCM_PROFILE"), Budget: os.Getenv("_CCM_BUDGET"),
		Ceiling: os.Getenv("_CCM_CEILING"), Sources: v046Lines(os.Getenv("_CCM_SOURCES")),
		PctRaw: os.Getenv("_CCM_PCT"), RawOut: strings.TrimSpace(os.Getenv("_CCM_MAXOUT")),
		No1m: os.Getenv("_CCM_NO1M") != "", Conf: os.Getenv("_CCM_PROFILE_CONF"),
		PctInEnv: os.Getenv("_CCM_PCT_IN_ENV") != "",
	})
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if len(rows) > 0 {
		fmt.Fprintln(out, strings.Join(rows, "\n"))
	}
	return 0
}

// v046HazardMsg is 6860fb5's util hazard-msg: _CCM_ROWS framed into one line.
func v046HazardMsg(_ []string, _ io.Reader, out, _ io.Writer) int {
	msg := HazardMessage(v046Lines(os.Getenv("_CCM_ROWS")), os.Getenv("_CCM_WITHHELD") == "1")
	if msg != "" {
		fmt.Fprintln(out, msg)
	}
	return 0
}

// v046AdviseJSON is 6860fb5's util advise-json: _CCM_ADVISE required.
func v046AdviseJSON(_ []string, _ io.Reader, out, errw io.Writer) int {
	msg, ok := os.LookupEnv("_CCM_ADVISE")
	if !ok {
		fmt.Fprintln(errw, "KeyError: '_CCM_ADVISE'")
		return 1
	}
	fmt.Fprintln(out, Advise(msg))
	return 0
}

// v046WithholdFatal is 6860fb5's util withhold-fatal: _CCM_SETTINGS is edited,
// _CCM_ROWS names the fatal models, _CCM_LAUNCHED the one session's model.
func v046WithholdFatal(_ []string, _ io.Reader, _, _ io.Writer) int {
	WithholdFatal(os.Getenv("_CCM_SETTINGS"), v046Lines(os.Getenv("_CCM_ROWS")), os.Getenv("_CCM_LAUNCHED"))
	return 0
}

// v046LaunchDiagnostics is 6860fb5's util launch-diagnostics: <keys...> -- <sources...>
// from the _CCM_ env, NUL-framed `<kind>\t<text>` records on stdout.
func v046LaunchDiagnostics(args []string, _ io.Reader, out, _ io.Writer) int {
	split := len(args)
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	in := DiagIn{
		Keys: args[:split], Settings: os.Getenv("_CCM_SETTINGS"),
		ProfileConf: os.Getenv("_CCM_PROFILE_CONF"), Profile: os.Getenv("_CCM_PROFILE"),
		EnvPct: os.Getenv("_CCM_ENV_PCT"), MaxOut: os.Getenv("_CCM_MAXOUT"),
		Budget: os.Getenv("_CCM_BUDGET"), Policy: os.Getenv("_CCM_POLICY"),
		PolicyPct: os.Getenv("_CCM_POLICY_PCT"), HdrKey: os.Getenv("_CCM_HDR_KEY"),
		Ceiling:  os.Getenv("_CCM_CEILING"),
		PctInEnv: os.Getenv("_CCM_PCT_IN_ENV") == "1", No1m: os.Getenv("_CCM_NO1M") != "",
		Hazard: os.Getenv("_CCM_HAZARD") == "1", Withheld: os.Getenv("_CCM_WITHHELD") == "1",
	}
	if split < len(args) {
		in.Paths = args[split+1:]
	}
	for _, r := range LaunchDiagnostics(in) {
		fields := []string{r.Kind}
		if r.Text != "" {
			fields = append(fields, r.Text)
		}
		if strings.ContainsRune(strings.Join(fields, ""), 0) {
			continue
		}
		io.WriteString(out, strings.Join(fields, "\t")+"\x00")
	}
	return 0
}

// v046ListingHasUnsized is 6860fb5's util listing-has-unsized: <profile>, the
// listing on stdin. 0 means the sizer is worth starting.
func v046ListingHasUnsized(args []string, in io.Reader, _, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	conf, err := ctxwindow.ProfilePath(args[0])
	if err != nil {
		return 0
	}
	if ListingHasUnsized(conf, string(readAll(in))) {
		return 0
	}
	return 1
}

// v046UnwireStatusline is 6860fb5's util unwire-statusline: <file>, the commands
// ccmorph once wired in _CCM_SL_OWNED.
func v046UnwireStatusline(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	if UnwireStatusline(args[0], v046Lines(os.Getenv("_CCM_SL_OWNED"))...) != nil {
		return 1
	}
	return 0
}

// v046LaunchWindow is 6860fb5's util launch-window: <profile> <model> [sources...].
func v046LaunchWindow(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) < 2 {
		return 2
	}
	fmt.Fprintln(out, strings.Join(LaunchWindow(args[0], args[1], args[2:]), "\t"))
	return 0
}
