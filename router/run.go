package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

const abortedAnswer = "Error: aborted"

// WorkerEnv marks a delegated worker; the hooks leave its own Agent calls unrouted.
const WorkerEnv = "CCMORPH_ROUTER_WORKER"

type Request struct {
	Prompt                 string
	Kind, Tier             string
	Profile, Model, Effort string
	Family                 string
	NoDecider, Refresh     bool
	Timeout                float64           // seconds for the whole dispatch, retry included; 0 keeps router.json's
	ClaudeArgs             []string          // passed to the worker's claude before -p
	Only                   []string          // profiles this route may use; nil for any
	Keep                   func(Option) bool // when set, only options it accepts may be picked
	Deadline               time.Time         // the hook's budget: cached quota, no decider past it
}

type Decision struct {
	Pick      *Option  `json:"pick"`
	Task      Task     `json:"task"`
	Shortlist []Option `json:"shortlist"`
	Dropped   []string `json:"dropped"`
	Notes     []string `json:"notes"`
}

// Decide picks where one task runs.
func Decide(c Config, r Request) (Decision, error) {
	// The ledger writes that follow share the route's budget: a hook's own
	// deadline, else the bounded default.
	armLock(r.Deadline)
	var d Decision
	decider := c.Decider
	if r.NoDecider {
		decider = nil
	}
	if r.Kind != "" && r.Tier != "" {
		d.Task = Task{Kind: r.Kind, Tier: r.Tier, Source: "given"}
	} else {
		timeout := 10 * time.Second
		if !r.Deadline.IsZero() {
			timeout = min(timeout, time.Until(r.Deadline)/2)
		}
		var note string
		d.Task, note = Classify(r.Prompt, decider, timeout)
		if note != "" {
			d.Notes = append(d.Notes, note)
		}
		if r.Kind != "" {
			d.Task.Kind = r.Kind
		}
		if r.Tier != "" {
			d.Task.Tier, d.Task.Source = r.Tier, "given"
		}
	}
	if r.Tier == "" {
		d.Task = Capped(d.Task, c.TierCap)
	}
	if r.Profile != "" && r.Model != "" {
		// A named profile and model is the caller's choice: run it as named, past
		// every routing filter (disabled, reserved, advisor-only), or fail loudly.
		if r.Family != "" { // a family cannot narrow a pair already named; never drop it silently
			return d, fmt.Errorf("family %q cannot narrow a named profile and model; name the family or the model, not both", r.Family)
		}
		if !slices.Contains(profileNames(), r.Profile) {
			return d, fmt.Errorf("no ccmorph profile %q", r.Profile)
		}
		if r.Refresh { // the pair runs as named; the caller still asked for a live reading now
			readQuota(profileNames(), true, false)
		}
		effort := r.Effort
		if effort == "" {
			effort = Efforts[d.Task.Tier]
		}
		d.Pick = &Option{Profile: r.Profile, Model: r.Model, Effort: effort}
		d.Shortlist = []Option{*d.Pick}
		return d, nil
	}
	profiles, notes := Profiles(c, r.Refresh, !r.Deadline.IsZero(), r.Only)
	if len(profiles) == 0 {
		return d, errors.New("no ccmorph profiles")
	}
	d.Notes = append(d.Notes, notes...)
	pin := Pin{Profile: r.Profile, Model: r.Model, Effort: r.Effort, Order: c.Order}
	if only, ok := c.Only[d.Task.Tier]; ok && r.Model == "" {
		pin.Only = append([]string{}, only...) // no fallback outside them; [] allows none
	}
	opts, dropped, err := Options(d.Task, profiles, labeler(c, profiles, decider, r.Deadline), c.Kinds, nowSec(), pin)
	if err != nil {
		return d, err
	}
	if r.Family != "" {
		opts = slices.DeleteFunc(opts, func(o Option) bool { return o.Family != r.Family })
	}
	if r.Keep != nil {
		opts = slices.DeleteFunc(opts, func(o Option) bool { return !r.Keep(o) })
	}
	d.Dropped = dropped
	d.Shortlist = Choose(opts, c.Order)
	if len(d.Shortlist) > 0 {
		d.Pick = &d.Shortlist[0]
	}
	d.Shortlist = d.Shortlist[:min(8, len(d.Shortlist))]
	return d, nil
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func reserve(id string, d Decision, brief string, extra Row) {
	row := Row{"id": id, "ts": nowSec(), "kind": d.Task.Kind, "tier": d.Task.Tier, "tier_source": d.Task.Source,
		"profile": d.Pick.Profile, "model": d.Pick.Model, "family": d.Pick.Family, "effort": d.Pick.Effort,
		"brief": brief, "outcome": nil}
	for k, v := range extra {
		row[k] = v
	}
	ledgerAppend(row)
}

// Outcome is one dispatch as its caller sees it: the worker's stdout unchanged,
// the router's own account of a failure, complete()'s notes, and the exit
// status (the worker's, 124 past the deadline, 1 when no worker ran).
type Outcome struct {
	Out     string
	Err     string // "" on success
	Hints   []string
	Status  int
	Aborted bool // the caller cancelled
}

// Delegate is Dispatch as one answer string, the shape the MCP tool returns:
// the worker's answer, or "Error: ..." with the worker's words, plus the notes.
func Delegate(ctx context.Context, r Request, cwd string) string {
	o := Dispatch(ctx, r, cwd)
	switch {
	case o.Aborted:
		return abortedAnswer // the MCP caller expects this exact string, with no note
	case o.Err != "":
		return withHints("Error: "+o.Err, o.Hints)
	}
	return withHints(o.Out, o.Hints)
}

// Dispatch runs delegated work through `ccmorph run`. The request carries the
// caller's whole say — kind, tier, pins, family, decider and refresh included —
// and every one of them routes this dispatch as surely as it routes
// `dispatch --dry-run`. One deadline bounds routing and every attempt. An
// account refusal (quota, auth) is retried once at the same tier, and only when
// the worker ran no tool: a retry never repeats a side effect. A pinned profile
// or model is retried as named, never swapped. Cancelling ctx kills the worker.
func Dispatch(ctx context.Context, r Request, cwd string) Outcome {
	fail := func(msg string) Outcome { return Outcome{Err: msg, Status: 1} }
	if strings.TrimSpace(r.Prompt) == "" {
		return fail("the router needs a non-empty prompt")
	}
	if r.Kind != "" && !slices.Contains(Kinds, r.Kind) || r.Tier != "" && Efforts[r.Tier] == "" {
		return fail("the router needs a valid kind and tier")
	}
	prompt := r.Prompt
	c, err := LoadConfig()
	if err != nil {
		return fail(ConfigPath() + ": " + err.Error())
	}
	if r.Timeout > 0 {
		c.Timeout = r.Timeout
	}
	if r.NoDecider { // no decider call at all, the result check included: a caller that judges the outcome itself asks for this
		c.Decider = nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout*float64(time.Second)))
	defer cancel()
	deadline, _ := ctx.Deadline()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return fail("cwd is not a directory: " + cwd)
	}
	var o Outcome
	var attempts []string
	source := ""
	for range 2 {
		if ctx.Err() == context.DeadlineExceeded { // checked again before a retry's worker starts
			o.Status = 124
			attempts = append(attempts, "the deadline passed before a worker could start")
			break
		}
		d, err := decideBy(deadline, c, r)
		if err == errStopped {
			o.Status = 124
			attempts = append(attempts, "routing ran past the deadline")
			break
		}
		if err != nil {
			attempts = append(attempts, err.Error())
			o.Status = 1
			break
		}
		r.Kind, r.Tier = d.Task.Kind, d.Task.Tier
		if source == "" {
			source = d.Task.Source
		}
		d.Task.Source = source // a retry keeps who chose the tier
		if d.Pick == nil {
			miss := "no next available profile at the same tier"
			if r.Family != "" {
				miss = "no next available profile serves family " + r.Family + " at the same tier"
			}
			attempts = append(attempts, miss)
			if o.Status == 0 {
				o.Status = 1
			}
			break
		}
		job := newID("delegate")
		reserve(job, d, prompt, nil)
		w := runWorker(ctx, d.Pick, prompt, cwd, job, deadline, r.ClaudeArgs)
		if ctx.Err() == context.DeadlineExceeded {
			w.status = 124
		} else if ctx.Err() != nil {
			ledgerUpdate(job, Row{"outcome": "error", "failure": "aborted"})
			return Outcome{Aborted: true, Out: w.out, Status: 130}
		}
		hard := ""
		switch {
		case w.status == 124 || w.status == 137:
			hard = "timeout"
		default:
			hard = hardFailure(w.answer+"\n"+w.errText, w.status != 0, "")
		}
		if hard == "" && w.status != 0 {
			hard = "error"
		}
		o.Out, o.Status = w.out, w.status
		if hard != "" {
			if hint := complete(c, job, "", hard); hint != "" {
				o.Hints = append(o.Hints, hint)
			}
			ledgerUpdate(job, Row{"outcome": "error", "failure": hard, "exit_code": w.status})
			attempts = append(attempts, fmt.Sprintf("%s/%s: %s (exit %d)", d.Pick.Profile, d.Pick.Model, hard, w.status))
			if last := strings.TrimSpace(w.answer + "\n" + w.errText); last != "" {
				attempts = append(attempts, "worker said:\n"+last)
			}
			if o.Status == 0 {
				o.Status = 1
			}
			if hard != "quota" && hard != "auth" || w.toolRan {
				break // the model answered, ran out of time, or acted: a second run would pay twice or repeat it
			}
			continue
		}
		if hint := complete(c, job, w.answer, ""); hint != "" {
			o.Hints = append(o.Hints, hint)
		}
		ledgerUpdate(job, Row{"exit_code": w.status})
		return o
	}
	if !o.Aborted {
		o.Err = strings.Join(attempts, "; ")
	}
	return o
}

var errStopped = errors.New("stopped")

// decideBy is Decide bounded by the dispatch deadline: a slow quota refresh or
// decider call never holds a dispatch past it. A caller's cancel lets routing
// finish, so the ledger records the pick the aborted worker would have run.
// Decide itself runs on past the deadline; its result is dropped.
func decideBy(deadline time.Time, c Config, r Request) (Decision, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	type result struct {
		d   Decision
		err error
	}
	ch := make(chan result, 1)
	go func() { d, err := Decide(c, r); ch <- result{d, err} }()
	select {
	case x := <-ch:
		return x.d, x.err
	case <-ctx.Done():
		return Decision{}, errStopped
	}
}

// withHints appends complete()'s notes to an answer: a dispatch or MCP caller
// has no PostToolUse additionalContext, so the verdict rides on the answer.
// Without a note the answer stays byte-identical.
func withHints(answer string, hints []string) string {
	if len(hints) == 0 {
		return answer
	}
	return answer + "\n" + strings.Join(hints, "\n")
}

// workerRun is one attempt: its stdout as the caller asked for it, the final
// answer within it, its stderr, its exit status, and whether it ran a tool.
type workerRun struct {
	out, answer, errText string
	status               int
	toolRan              bool
}

// runWorker runs one attempt in its own process group, killed whole on timeout,
// cancel or exit: ccmorph can leave children behind. A caller's CCMORPH_TASK
// stays the worker's, so its spend lands on the caller's tag; CCMORPH_DEADLINE
// tells the worker when it is killed, in epoch seconds. Unless the caller chose
// an output format, the worker streams JSON so the router sees every tool call,
// and the caller gets the final answer alone, as plain -p prints it.
func runWorker(ctx context.Context, pick *Option, prompt, cwd, job string, deadline time.Time, claudeArgs []string) workerRun {
	argv := []string{"run", pick.Profile, "--model", pick.Model, "--", "--effort", pick.Effort}
	stream := !slices.ContainsFunc(claudeArgs, func(a string) bool { return a == "--output-format" || strings.HasPrefix(a, "--output-format=") })
	if stream {
		argv = append(argv, "--output-format", "stream-json", "--verbose")
	}
	argv = append(append(argv, claudeArgs...), "-p", prompt)
	cmd := exec.Command("ccmorph", argv...)
	cmd.Dir = cwd
	task := os.Getenv("CCMORPH_TASK")
	if task == "" {
		task = job
	}
	cmd.Env = append(os.Environ(), "CCMORPH_TASK="+task, WorkerEnv+"=1", fmt.Sprintf("CCMORPH_DEADLINE=%d", deadline.Unix()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return workerRun{errText: err.Error(), status: 1}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	w := workerRun{}
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			w.status = ee.ExitCode()
		} else if err != nil {
			w.status = 1
		}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		w.status = 124
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	w.out, w.answer, w.errText = out.String(), out.String(), errb.String()
	if !stream {
		w.toolRan = true // a format the caller chose hides the tool calls: never assume none ran
		return w
	}
	if answer, toolRan, ok := readStream(w.out); ok {
		w.out, w.answer, w.toolRan = answer, answer, toolRan
	}
	return w
}

// readStream reads Claude Code's stream-json output: the result event's text,
// and whether any assistant turn called a tool. ok is false when the output is
// not a stream (a worker that died before claude started, or another program).
func readStream(raw string) (answer string, toolRan, ok bool) {
	for _, line := range strings.Split(raw, "\n") {
		var ev struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		ok = true
		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				toolRan = toolRan || b.Type == "tool_use"
			}
		case "result":
			answer = ev.Result
		}
	}
	return answer, toolRan, ok
}

var (
	failurePatterns = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"quota", regexp.MustCompile(`(?i)\b(quota|rate.?limit|usage limit|429)\b`)},
		// The bare word "auth" is a refusal only beside failed/required, the
		// context errorOpening names below: Claude Code prints a routine startup
		// warning naming an "auth source" on every run, and its lone word would
		// otherwise hide the real failure (a gateway 500, a TLS timeout) behind auth.
		{"auth", regexp.MustCompile(`(?i)\b(authentication|authorization|unauthorized|forbidden|401|403)\b|\bauth\s+(?:failed|required)\b`)},
		{"timeout", regexp.MustCompile(`(?i)\b(timed?\s*out|timeout|deadline exceeded)\b`)},
	}
	errorOpening = regexp.MustCompile(`(?i)^\s*(?:(?:api )?(?:error|failed)\s*:|(?:quota|rate.?limit|usage limit) (?:exceeded|reached)|` +
		`(?:auth(?:entication|orization)?) (?:failed|required)|unauthorized\b|forbidden\b|(?:request )?(?:timed?\s*out|timeout)\b)`)
)

// hardFailure names a failed run: quota and auth are the account's refusal,
// anything else one model's failure. "" is a real answer.
func hardFailure(text string, isError bool, status string) string {
	isError = isError || errorOpening.MatchString(text)
	for _, p := range failurePatterns {
		if p.re.MatchString(status) || isError && p.re.MatchString(text) {
			return p.kind
		}
	}
	if strings.TrimSpace(text) == "" {
		return "empty"
	}
	return ""
}

// complete settles a route's outcome and returns a hint for the caller, if any.
func complete(c Config, id, text, hard string) string {
	var row Row
	for _, r := range readRows(ledgerPath()) {
		if r.matches(id) {
			row = r
			break
		}
	}
	if row == nil || row["outcome"] != nil {
		return ""
	}
	profile, model := fmt.Sprint(row["profile"]), fmt.Sprint(row["model"])
	now := nowSec()
	var ps []Profile
	if hard == "quota" || hard == "auth" {
		ps, _ = Profiles(c, false, true, []string{profile})
	}
	// Only a subscription's refusal is the account's: a gateway's 403 or 429 names
	// the one upstream behind that model, and its other models still answer.
	if hard != "" && !(len(ps) == 1 && ps[0].Class == "subscription") {
		// One broken model must not pause its siblings; an error is retried, not a verdict.
		markUnavailable(profile+"/"+model, now+modelBackoff)
		ledgerUpdate(id, Row{"outcome": hard})
		return fmt.Sprintf("router: %s/%s backed off after %s; retry the same brief at the same tier.", profile, model, hard)
	}
	if hard != "" {
		until := now + windowLength["5h"]
		tightest := math.Inf(1)
		for _, p := range ps {
			for _, w := range p.Windows {
				if pace := 1 - (w.ResetsAt-now)/w.Length - w.Used; w.ResetsAt > now && pace < tightest {
					tightest, until = pace, w.ResetsAt
				}
			}
		}
		markUnavailable(profile, until)
		ledgerUpdate(id, Row{"outcome": hard})
		return fmt.Sprintf("router: %s unavailable until reset after %s; retry the same brief at the same tier on the next profile.", profile, hard)
	}
	var passed *bool
	if c.Decider != nil {
		passed = checkResult(c.Decider, fmt.Sprint(row["brief"]), text)
	}
	outcome := "completed"
	if passed != nil && !*passed {
		outcome = "quality_fail"
	}
	ledgerUpdate(id, Row{"outcome": outcome, "quality": passed})
	if outcome == "completed" {
		return ""
	}
	if row["escalated"] == true {
		return "router: quality failed after the one escalation; return the failure to the main agent."
	}
	families := c.Kinds[fmt.Sprint(row["kind"])]
	if len(families) == 0 {
		return fmt.Sprintf("router: quality fail; %s has no rank-1 family configured. Return to the main agent.", row["kind"])
	}
	return fmt.Sprintf("router: quality fail. Escalate once: rerun the same %s brief at top tier on family %s. "+
		"If this was already the escalation, return the failure to the main agent.", row["kind"], families[0])
}
