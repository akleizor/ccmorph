package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const Usage = "ccmorph router [on|off|status|dispatch [--dry-run]|report]"

// TaskHelp is `ccmorph router dispatch --help`: every flag stated with the
// effect it has, on a run and on --dry-run, because a flag that is parsed and
// then means nothing reads like a bug (GH-694).
const TaskHelp = `usage: ccmorph router dispatch [--dry-run] [flags] [brief] [-- claude flags]

The brief is the words, or stdin when there are none; a word that starts
with - is read as a flag, so give such a brief on stdin. dispatch routes it,
runs it on the picked profile through ccmorph run and prints the worker's
stdout unchanged: its final answer, or the output format it was given. The
router's own notes go to stderr; the exit status is the worker's, 124 past
the deadline. With --dry-run it prints the pick and reserves it in
the ledger without running anything.

  --kind K      writing, search, x, mechanical, code, design, image_gen or vision; with --tier it skips classification
  --tier T      small, mid or top; the effort follows the tier
  --profile P   route only on profile P, never another
  --model M     route only on served model M, never another
  --family F    restrict the shortlist to models of family F; when none
                qualifies dispatch fails naming F, --dry-run exits 3; refused
                when --profile and --model are both named
  --no-decider  never ask the decider: kind and tier come from the keyword
                rules, an unlabeled model is labeled by its name, and the
                finished answer is not scored (the caller judges it)
  --refresh     read quota live now instead of the stored snapshot, which is
                served up to 5 minutes old; a named --profile and --model too
  --effort E    run at effort E instead of the tier's
  --timeout S   kill the worker after S seconds, a retry included (default:
                router.json's timeout); the worker reads its end, in epoch
                seconds, from CCMORPH_DEADLINE
  --cwd DIR     run the worker in DIR (default: this directory)
  -- FLAGS      pass FLAGS to the worker's claude; --model, --effort, -p
                and a second -- belong to the dispatch and are refused there
A failed run is retried once, on the next profile, only when the account
refused it (quota or auth) before the worker ran any tool. A caller's CCMORPH_TASK tags the worker's spend.
  --dry-run     print the pick and reserve it in the ledger; run nothing
  --json        with --dry-run: the decision as JSON
  --explain     with --dry-run: print the shortlist and every dropped model
`

// Main is `ccmorph router ...`. stdin carries the prompt for dispatch when no
// words do, and the event for the hooks.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	verb := "status"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	switch verb {
	case "on", "off":
		if len(args) > 0 {
			return refuse(stderr, args[0])
		}
		if err := SetEnabled(verb == "on"); err != nil {
			fmt.Fprintln(stderr, "ccmorph: router:", err)
			return 1
		}
		if verb == "on" {
			fmt.Fprintln(stdout, "Router on. New sessions route subagents; open sessions keep running as they started.")
		} else {
			fmt.Fprintln(stdout, "Router off. New sessions start without it.")
		}
		return 0
	case "status":
		if len(args) > 0 {
			return refuse(stderr, args[0])
		}
		return status(stdout, stderr)
	case "dispatch":
		return task(args, stdin, stdout, stderr)
	case "report":
		return report(args, stdout, stderr)
	case "mcp":
		return MCPMain()
	case "hook":
		event := ""
		if len(args) > 0 {
			event = args[0]
		}
		switch event {
		case "pre":
			PreToolUse(stdin, stdout)
		case "post":
			PostToolUse(stdin, stdout)
		case "stop":
			SubagentStop(stdin, stdout)
		}
		return 0 // a hook never fails the tool call
	}
	return refuse(stderr, verb)
}

// refuse names the word the router did not expect, with its usage line, before
// anything runs: a typo must never act or pass as success (#784).
func refuse(stderr io.Writer, word string) int {
	if strings.HasPrefix(word, "-") {
		fmt.Fprintf(stderr, "ccmorph: router: unknown option '%s'\n", word)
	} else {
		fmt.Fprintf(stderr, "ccmorph: router: unexpected argument '%s'\n", word)
	}
	fmt.Fprintln(stderr, "usage: "+Usage)
	return 2
}

// needsValue refuses a flag given without its value, or with another flag in
// the value's place, naming the flag.
func needsValue(stderr io.Writer, flag string) int {
	fmt.Fprintf(stderr, "ccmorph: router: %s needs a value\n", flag)
	fmt.Fprintln(stderr, "usage: "+Usage)
	return 2
}

// needsValueNotOption refuses a value flag with another flag where its value
// belongs, naming both: the value flag that lost its value, and the option a
// caller probably mistyped (#784).
func needsValueNotOption(stderr io.Writer, flag, other string) int {
	fmt.Fprintf(stderr, "ccmorph: router: %s needs a value, not the option '%s'\n", flag, other)
	fmt.Fprintln(stderr, "usage: "+Usage)
	return 2
}

func status(stdout, stderr io.Writer) int {
	c, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(stderr, "ccmorph: router:", ConfigPath()+":", err)
		return 1
	}
	state := "off"
	if c.Enabled {
		state = "on"
	}
	fmt.Fprintf(stdout, "Router %s (%s)\n", state, ConfigPath())
	ps, notes := Profiles(c, false, false, nil)
	now := nowSec()
	for _, p := range ps {
		mark := ""
		switch {
		case p.Reserved:
			mark = " reserved"
		case !p.Enabled:
			mark = " disabled"
		case p.UnavailableUntil > now:
			mark = " unavailable"
		}
		pace := "no reading"
		if len(p.Windows) > 0 {
			pace = fmt.Sprintf("slack %+.1f points", Slack(p, now, ""))
		}
		fmt.Fprintf(stdout, "  %-12s %-12s %s%s\n", p.Name, p.Class, pace, mark)
	}
	for _, n := range notes {
		fmt.Fprintln(stdout, "  note: "+n)
	}
	return 0
}

func task(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var r Request
	var words []string
	asJSON, explain, dryRun, cwd := false, false, false, ""
	// The flags that take a value. Nothing, or another flag, in the value's
	// place is refused: --profile --dry-run would otherwise run for real (#784).
	values := map[string]*string{"--effort": &r.Effort, "--kind": &r.Kind, "--tier": &r.Tier,
		"--profile": &r.Profile, "--model": &r.Model, "--family": &r.Family, "--cwd": &cwd}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if dst, ok := values[a]; ok {
			if i+1 >= len(args) || args[i+1] == "--" {
				return needsValue(stderr, a)
			}
			if strings.HasPrefix(args[i+1], "-") {
				return needsValueNotOption(stderr, a, args[i+1])
			}
			i++
			*dst = args[i]
			continue
		}
		switch a {
		case "--": // the rest go to the worker's claude
			r.ClaudeArgs = args[i+1:]
			i = len(args)
			for _, f := range r.ClaudeArgs {
				k, _, _ := strings.Cut(f, "=")
				// A second -- would end claude's options and turn the router's -p and prompt into words.
				bundle := len(f) > 2 && f[0] == '-' && f[1] != '-' && strings.ContainsRune(f[1:], 'p')
				if f == "--" || bundle || slices.Contains([]string{"--model", "--effort", "-p", "--print"}, k) {
					fmt.Fprintln(stderr, "ccmorph: router: "+f+" belongs to the dispatch, not to the claude flags")
					return 2
				}
			}
		case "--timeout":
			if i+1 >= len(args) || args[i+1] == "--" {
				return needsValue(stderr, "--timeout")
			}
			arg := args[i+1]
			v, err := strconv.ParseFloat(arg, 64)
			// A flag where the number belongs is named as the option it is, not
			// swallowed as an invalid duration (#784). -1 still parses, so a
			// negative number keeps the invalid-value answer that names it.
			if err != nil && strings.HasPrefix(arg, "-") {
				return needsValueNotOption(stderr, "--timeout", arg)
			}
			if err != nil || !(v > 0) || math.IsInf(v, 0) {
				fmt.Fprintf(stderr, "ccmorph: router: --timeout has invalid value %q; it takes a positive number of seconds\n", arg)
				fmt.Fprintln(stderr, "usage: "+Usage)
				return 2
			}
			i++
			r.Timeout = v
		case "-h", "--help":
			fmt.Fprint(stdout, TaskHelp)
			return 0
		case "--dry-run": // pick only, run nothing
			dryRun = true
		case "--json":
			asJSON = true
		case "--explain":
			explain = true
		case "--no-decider":
			r.NoDecider = true
		case "--refresh":
			r.Refresh = true
		default:
			if strings.HasPrefix(a, "-") {
				return refuse(stderr, a)
			}
			words = append(words, a)
		}
	}
	r.Prompt = strings.Join(words, " ")
	if r.Prompt == "" {
		b, _ := io.ReadAll(stdin)
		r.Prompt = string(b)
	}
	if r.Kind != "" && !slices.Contains(Kinds, r.Kind) || r.Tier != "" && Efforts[r.Tier] == "" {
		fmt.Fprintln(stderr, "ccmorph: router: --kind is one of", strings.Join(Kinds, ", "), "and --tier one of", strings.Join(Tiers, ", "))
		return 2
	}
	if !dryRun {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		// The worker's stdout is the caller's to parse, so the router's own words go to stderr.
		o := Dispatch(ctx, r, cwd)
		fmt.Fprint(stdout, o.Out)
		for _, h := range o.Hints {
			fmt.Fprintln(stderr, "ccmorph: router: "+h)
		}
		if o.Err != "" {
			fmt.Fprintln(stderr, "ccmorph: router: "+o.Err)
		}
		if o.Aborted {
			return 130
		}
		return o.Status
	}
	c, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(stderr, "ccmorph: router:", err)
		return 1
	}
	d, err := Decide(c, r)
	if err != nil {
		fmt.Fprintln(stderr, "ccmorph: router:", err)
		return 1
	}
	if d.Pick != nil {
		reserve(newID("route"), d, r.Prompt, nil)
	}
	if asJSON {
		e := json.NewEncoder(stdout)
		e.SetIndent("", "  ")
		_ = e.Encode(d)
	} else {
		show(stdout, d, explain)
	}
	if d.Pick == nil {
		return 3
	}
	return 0
}

func show(w io.Writer, d Decision, explain bool) {
	if p := d.Pick; p != nil {
		fmt.Fprintf(w, "→ %s · %s · effort %s\n  %s / %s · %s · slack %.1f points\n", p.Profile, p.Model, p.Effort, d.Task.Kind, d.Task.Tier, p.Class, p.Slack)
	} else {
		fmt.Fprintln(w, "no available profile serves this task's family and tier")
	}
	for _, n := range d.Notes {
		fmt.Fprintln(w, "  note: "+n)
	}
	if explain || d.Pick == nil {
		for _, o := range d.Shortlist {
			fmt.Fprintf(w, "  %s · %s · slack %.1f · rank %d\n", o.Profile, o.Model, o.Slack, o.Rank)
		}
		for _, r := range d.Dropped[:min(12, len(d.Dropped))] {
			fmt.Fprintln(w, "  dropped: "+r)
		}
	}
}

// report checks real traffic against the router's promises; non-zero when one fails.
func report(args []string, stdout, stderr io.Writer) int {
	since := "24h"
	for i := 0; i < len(args); i++ {
		if args[i] != "--since" {
			return refuse(stderr, args[i])
		}
		if i+1 >= len(args) || args[i+1] == "--" {
			return needsValue(stderr, "--since")
		}
		if strings.HasPrefix(args[i+1], "-") {
			return needsValueNotOption(stderr, "--since", args[i+1])
		}
		i++
		since = args[i]
	}
	m := sinceRe.FindStringSubmatch(since)
	if m == nil {
		fmt.Fprintln(stderr, "ccmorph: router: --since is a duration such as 24h, 30m or 7d")
		return 2
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	cut := nowSec() - n*map[string]float64{"h": 3600, "m": 60, "d": 86400}[m[2]]
	c, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(stderr, "ccmorph: router:", err)
		return 1
	}
	ps, _ := Profiles(c, false, false, nil)
	var rows []Row
	calls := map[string]int{}
	for _, r := range readRows(ledgerPath()) {
		if ts, _ := r["ts"].(float64); ts >= cut {
			rows = append(rows, r)
			calls[fmt.Sprint(r["profile"])]++
		}
	}
	var slacks []float64
	var failures []string
	now := nowSec()
	fmt.Fprintln(stdout, "profile      calls slack (points)")
	for _, p := range ps {
		slack := "unknown"
		if len(p.Windows) > 0 {
			slack = fmt.Sprintf("%.1f", Slack(p, now, ""))
		}
		fmt.Fprintf(stdout, "%-12s %5d %s\n", p.Name, calls[p.Name], slack)
		switch {
		case p.Reserved && calls[p.Name] > 0:
			failures = append(failures, "reserved profile "+p.Name+" took calls")
		case !p.Reserved && p.Enabled && p.Class == "subscription":
			if calls[p.Name] == 0 {
				failures = append(failures, "subscription "+p.Name+" took no calls")
			}
			if len(p.Windows) > 0 {
				slacks = append(slacks, Slack(p, now, ""))
			}
		}
	}
	if len(slacks) > 0 && slices.Max(slacks)-slices.Min(slacks) > 20 {
		failures = append(failures, "subscription slack spread exceeds 20 points")
	}
	finished, failed := 0, 0
	for _, r := range rows {
		ranked := c.Kinds[fmt.Sprint(r["kind"])]
		if r["tier"] == "top" && len(ranked) > 0 && !slices.Contains(ranked[:min(2, len(ranked))], fmt.Sprint(r["family"])) {
			failures = append(failures, fmt.Sprintf("top-tier route %v missed its kind's rank-1/2 family", r["id"]))
		}
		if r["outcome"] != nil {
			finished++
			if r["outcome"] != "completed" {
				failed++
			}
		}
	}
	if failed*5 > finished {
		failures = append(failures, fmt.Sprintf("%d/%d finished routes failed (exceeds 20%%)", failed, finished))
	}
	seen, bad := 0, 0
	for _, r := range readRows(filepath.Join(routerDir(), "outcomes.jsonl")) {
		if ts, _ := r["ts"].(float64); r["event"] == "delegate" && ts >= cut {
			seen++
			if r["result"] != "ok" {
				bad++
			}
		}
	}
	if bad*5 > seen {
		failures = append(failures, fmt.Sprintf("%d/%d delegate calls returned an error or were aborted (exceeds 20%%)", bad, seen))
	}
	for _, f := range slices.Compact(failures) {
		fmt.Fprintln(stdout, "FAIL: "+f)
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}
