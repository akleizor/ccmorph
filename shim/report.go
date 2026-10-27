package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// recordOutcome appends one task outcome beside the usage rows; the join key is
// the task id.
func recordOutcome(task, result string) {
	if !taskRE.MatchString(task) || !slices.Contains(outcomes, result) {
		die(fmt.Sprintf("usage: %s outcome <task> %s", os.Args[0], strings.Join(outcomes, "|")))
	}
	dir := stateDir()
	os.MkdirAll(filepath.Dir(dir), 0o777)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		die(err.Error())
	}
	row := `{"task":` + string(jsonStr(task)) + `,"outcome":"` + result + `","timestamp":"` + isoNow() + `"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "outcomes.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		die(err.Error())
	}
	defer f.Close()
	f.WriteString(row)
}

func isoNow() string {
	now := time.Now().UTC()
	ts := now.Format("2006-01-02T15:04:05")
	if us := now.Nanosecond() / 1000; us != 0 {
		ts += fmt.Sprintf(".%06d", us)
	}
	return ts + "+00:00"
}

var isoLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04", "2006-01-02",
}

// parseTime is datetime.fromisoformat; a time with no zone is UTC.
func parseTime(v string) (time.Time, bool) {
	for _, l := range isoLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

func modelID(raw string) string { return strings.TrimSuffix(raw, "[1m]") }

type taskVerdict struct {
	outcome string
	when    time.Time
}

// report is the read-only spend window and verdict cohort; accepted costs span
// the whole task.
func report(days float64) {
	base := stateDir()
	now := time.Now().UTC()
	usage := func() { die(fmt.Sprintf("usage: %s report [--days N]", os.Args[0])) }
	if !(days > 0) || days > 739000 {
		usage()
	}
	since := now.Add(-time.Duration(days * 24 * float64(time.Hour)))
	total, unknown := 0.0, 0
	byModel := map[string]float64{}
	type order struct {
		attempt int
		when    time.Time
	}
	final := map[string]order{}
	taskCost := map[string]float64{}
	unpriced := map[string]bool{}
	usageDir := filepath.Join(base, "usage")
	entries, _ := os.ReadDir(usageDir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue // the atomic writer's .tmp is not committed
		}
		data, err := os.ReadFile(filepath.Join(usageDir, name))
		if err != nil {
			continue
		}
		d := parseDoc(data)
		if d == nil {
			continue
		}
		ts, ok := d.str("timestamp")
		if !ok {
			continue
		}
		when, ok := parseTime(ts)
		us, hasUsage := d.last("usage")
		if !ok || !hasUsage || d.b[us.Start] != '{' || when.After(now) {
			continue
		}
		attempt := 1
		if sp, ok := d.last("attempt"); ok {
			n, err := strconv.Atoi(string(d.b[sp.Start:sp.End]))
			if err != nil || n < 1 {
				continue
			}
			attempt = n
		}
		var cost *float64
		for _, kvp := range bucketsOf(d.b, us, true) {
			if kvp.key == "cost" {
				f, _ := strconv.ParseFloat(string(kvp.raw), 64)
				cost = &f
			}
		}
		if t, ok := d.str("task"); ok && taskRE.MatchString(t) {
			// Whole-task accounting is independent of the spend window. Equal
			// attempts are ordered by response time, never response-hash filenames.
			if o, seen := final[t]; !seen || attempt > o.attempt || (attempt == o.attempt && when.After(o.when)) {
				final[t] = order{attempt, when}
			}
			if sp, ok := d.last("complete"); cost == nil || !ok || string(d.b[sp.Start:sp.End]) != "true" {
				unpriced[t] = true
			}
			if cost != nil {
				taskCost[t] += *cost
			}
		}
		if when.Before(since) {
			continue
		}
		if cost == nil {
			unknown++
			continue
		}
		// New rows record the actual outbound model. Older rows only recorded
		// response_model; requested_model is never evidence of a served identity.
		model := ""
		if m, ok := d.str("served_model"); ok {
			model = modelID(m)
		}
		if model == "" {
			if m, ok := d.str("response_model"); ok {
				model = modelID(m)
			}
		}
		if model == "" {
			model = "(unknown)"
		}
		total += *cost
		byModel[model] += *cost
	}

	history := map[string][]taskVerdict{} // last valid row in file order is the verdict
	var taskOrder []string
	if data, err := os.ReadFile(filepath.Join(base, "outcomes.jsonl")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			d := parseDoc([]byte(line))
			if d == nil {
				continue
			}
			task, ok1 := d.str("task")
			outcome, ok2 := d.str("outcome")
			ts, ok3 := d.str("timestamp")
			when, ok4 := parseTime(ts)
			if !ok1 || !ok2 || !ok3 || !ok4 || !taskRE.MatchString(task) || !slices.Contains(outcomes, outcome) || when.After(now) {
				continue
			}
			if _, seen := history[task]; !seen {
				taskOrder = append(taskOrder, task)
			}
			history[task] = append(history[task], taskVerdict{outcome, when})
		}
	}
	pct := func(part, whole int) string {
		if whole == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", 100.0*float64(part)/float64(whole))
	}
	fmt.Printf("ccmorph routing report, last %s day(s)\n", fmtG(days))
	fmt.Printf("total spend $%.2f\n", total)
	fmt.Printf("unknown spend %d rows (no cost reported, not estimated)\n", unknown)
	fmt.Println("spend by served model")
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.SliceStable(models, func(i, j int) bool { return byModel[models[i]] > byModel[models[j]] })
	for _, m := range models {
		label := m
		if !printable(m) {
			label = string(jsonStr(m))
		}
		fmt.Printf("  %s  $%.2f\n", label, byModel[m])
	}
	fmt.Println("\ntasks with outcomes")
	var tasks []string
	for _, t := range taskOrder {
		if h := history[t]; !h[len(h)-1].when.Before(since) {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		return
	}
	var accepted, first, esc []string
	for _, t := range tasks {
		h := history[t]
		if h[len(h)-1].outcome == "accepted" {
			accepted = append(accepted, t)
		}
		for _, v := range h {
			if v.outcome == "escalated" {
				esc = append(esc, t)
				break
			}
		}
	}
	known := len(accepted) > 0
	spend := 0.0
	for _, t := range accepted {
		if _, ok := taskCost[t]; !ok || unpriced[t] {
			known = false
		}
		spend += taskCost[t]
		allAccepted := true
		for _, v := range history[t] {
			allAccepted = allAccepted && v.outcome == "accepted"
		}
		if o, ok := final[t]; ok && o.attempt == 1 && allAccepted {
			first = append(first, t)
		}
	}
	perTask := "n/a"
	if known {
		perTask = fmt.Sprintf("$%.2f", spend/float64(len(accepted)))
	}
	fmt.Printf("  accepted %d  %s/task  first-pass %s  escalated %s\n",
		len(accepted), perTask+"", pct(len(first), len(tasks)), pct(len(esc), len(tasks)))
}

// fmtG is Python's "{:g}".
func fmtG(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

var _ = regexp.MustCompile
