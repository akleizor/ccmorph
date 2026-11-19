package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// WorkerEnv marks a delegated worker: it does its own work, never another delegation.
const WorkerEnv = "CCMORPH_ROUTER_WORKER"

type Request struct {
	Prompt                 string
	Kind, Tier             string
	Profile, Model, Effort string
	Family                 string
	NoDecider, Refresh     bool
	Only                   []string  // profiles this route may use; nil for any
	Deadline               time.Time // the hook's budget: cached quota, no decider past it
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

// Delegate runs delegated work through `ccmorph run` and returns only its final
// answer. A hard failure is retried once at the same tier; a pinned profile or
// model is retried as named, never swapped. Cancelling ctx kills the worker.
func Delegate(ctx context.Context, prompt, kind, tier, profile, model, cwd string) string {
	if os.Getenv(WorkerEnv) == "1" {
		return "Error: you are a delegated worker; do the work yourself, not another delegation"
	}
	if strings.TrimSpace(prompt) == "" {
		return "Error: the router needs a non-empty prompt"
	}
	if kind != "" && !slices.Contains(Kinds, kind) || tier != "" && Efforts[tier] == "" {
		return "Error: the router needs a valid kind and tier"
	}
	c, err := LoadConfig()
	if err != nil {
		return "Error: " + ConfigPath() + ": " + err.Error()
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "Error: cwd is not a directory: " + cwd
	}
	r := Request{Prompt: prompt, Kind: kind, Tier: tier, Profile: profile, Model: model}
	var attempts []string
	source := ""
	for range 2 {
		d, err := Decide(c, r)
		if err != nil {
			return "Error: " + strings.Join(append(attempts, err.Error()), "; ")
		}
		r.Kind, r.Tier = d.Task.Kind, d.Task.Tier
		if source == "" {
			source = d.Task.Source
		}
		d.Task.Source = source // a retry keeps who chose the tier
		if d.Pick == nil {
			return "Error: " + strings.Join(append(attempts, "no next available profile at the same tier"), "; ")
		}
		job := newID("delegate")
		reserve(job, d, prompt, nil)
		out, errText, status := runWorker(ctx, c, d.Pick, prompt, cwd, job)
		if ctx.Err() != nil {
			ledgerUpdate(job, Row{"outcome": "error", "failure": "aborted"})
			return abortedAnswer
		}
		hard := ""
		switch {
		case status == 124 || status == 137:
			hard = "timeout"
		default:
			hard = hardFailure(out+errText, status != 0, "")
		}
		if hard == "" && status != 0 {
			hard = "error"
		}
		if hard != "" {
			complete(c, job, "", hard)
			ledgerUpdate(job, Row{"outcome": "error", "failure": hard, "exit_code": status})
			attempts = append(attempts, fmt.Sprintf("%s/%s: %s (exit %d)", d.Pick.Profile, d.Pick.Model, hard, status))
			continue
		}
		complete(c, job, out, "")
		ledgerUpdate(job, Row{"exit_code": status})
		return out
	}
	return "Error: " + strings.Join(attempts, "; ")
}

// runWorker runs one attempt in its own process group, killed whole on timeout,
// cancel or exit: ccmorph can leave children behind.
func runWorker(ctx context.Context, c Config, pick *Option, prompt, cwd, job string) (string, string, int) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.Command("ccmorph", "run", pick.Profile, "--model", pick.Model, "--", "--effort", pick.Effort, "-p", prompt)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CCMORPH_TASK="+job, WorkerEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return "", err.Error(), 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	status := 0
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			status = ee.ExitCode()
		} else if err != nil {
			status = 1
		}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		status = 124
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return out.String(), errb.String(), status
}

var (
	failurePatterns = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"quota", regexp.MustCompile(`(?i)\b(quota|rate.?limit|usage limit|429)\b`)},
		{"auth", regexp.MustCompile(`(?i)\b(auth(?:entication|orization)?|unauthorized|forbidden|401|403)\b`)},
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
	if hard != "" && hard != "quota" && hard != "auth" {
		// One broken model must not pause its siblings; an error is retried, not a verdict.
		markUnavailable(profile+"/"+model, now+modelBackoff)
		ledgerUpdate(id, Row{"outcome": hard})
		return fmt.Sprintf("router: %s/%s backed off after %s; retry the same brief at the same tier.", profile, model, hard)
	}
	if hard != "" {
		until := now + windowLength["5h"]
		ps, _ := Profiles(c, false, true, []string{profile})
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
