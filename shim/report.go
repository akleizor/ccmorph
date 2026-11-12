package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// recordOutcome appends one task outcome beside the usage rows; the join key is
// the task id.
func recordOutcome(task, result string) {
	if !taskRE.MatchString(task) || !slices.Contains(outcomes, result) {
		die(fmt.Sprintf("usage: %s outcome <task> %s", os.Args[0], strings.Join(outcomes, "|")))
	}
	dir := paths.StateDir()
	os.MkdirAll(filepath.Dir(dir), 0o777)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		die(err.Error())
	}
	row := `{"task":` + string(jsonStr(task)) + `,"outcome":"` + result + `","timestamp":"` + utcStamp(time.Now().UTC()) + `"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "outcomes.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		die(err.Error())
	}
	defer f.Close()
	f.WriteString(row)
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

// report is the spend window and verdict cohort; accepted costs span the whole
// task. Its read writes nothing, and after it the flat rows of a pre-shard
// store are moved into their day folders — a relocation, never a change to a
// row's numbers.
func report(days float64) {
	base := paths.StateDir()
	now := time.Now().UTC()
	usage := func() { die(fmt.Sprintf("usage: %s report [--days N]", os.Args[0])) }
	if !(days > 0) || days > 739000 {
		usage()
	}
	since := now.Add(-time.Duration(days * 24 * float64(time.Hour)))
	total, unknown := 0.0, 0
	estimatedTotal, estimatedRows := 0.0, 0
	byModel := map[string]float64{}
	type order struct {
		attempt int
		when    time.Time
	}
	final := map[string]order{}
	taskCost := map[string]float64{}
	unpriced := map[string]bool{}
	// The outcomes are read first: they name the tasks this window reports on,
	// and a task's cost is whole-task — its usage before the window counts, so
	// its row read has no lower bound. A run with no verdict in its window
	// opens only the day folders its window touches; one with a verdict reads
	// every folder, plus any rows still flat at the top level. The row's own
	// timestamp still decides the window.
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
	var tasks []string
	for _, t := range taskOrder {
		if h := history[t]; !h[len(h)-1].when.Before(since) {
			tasks = append(tasks, t)
		}
	}
	lo := since
	if len(tasks) > 0 {
		lo = time.Time{} // a task's earlier usage has no lower bound
	}
	usageDir := filepath.Join(base, "usage")
	paths := usageRowFiles(usageDir, lo, now)
	sort.Strings(paths)
	counted := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		d := parseDoc(data)
		if d == nil {
			d = parseDoc(nonFiniteToNull(data))
		}
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
		// A row read under two names (a flat copy and its day folder's, left by
		// a move that stopped halfway) is one response: count it once (#298).
		if key, ok := rowIdentity(d, ts); ok {
			if counted[key] {
				continue
			}
			counted[key] = true
		}
		var cost *float64
		for _, kvp := range bucketsOf(d.b, us, true) {
			if kvp.key == "cost" {
				f, _ := strconv.ParseFloat(string(kvp.raw), 64)
				cost = &f
			}
		}
		// The shim's own models.dev list price, when the upstream billed none: a
		// separate number under a separate key, never billed spend.
		var estimate *float64
		if cost == nil {
			if ms, _ := Object(d.b, us.Start); ms != nil {
				if sp, ok := lastMember(ms, "estimated_cost"); ok {
					if f, fin := finiteNumber(d.b[sp.Start:sp.End]); fin && f >= 0 && f < costLimit {
						estimate = &f
					}
				}
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
			if estimate != nil {
				estimatedTotal += *estimate
				estimatedRows++
				continue
			}
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

	// The window is read and the totals are in: put the flat rows this
	// pre-shard store still holds into their day folders, so the next read
	// pays only its window. This run's numbers came from the rows as they
	// were; the move relocates them, it never changes one.
	moveFlatRows()

	pct := func(part, whole int) string {
		if whole == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", 100.0*float64(part)/float64(whole))
	}
	fmt.Printf("ccmorph routing report, last %s day(s)\n", fmtG(days))
	fmt.Printf("total spend $%.2f\n", total)
	fmt.Printf("unknown spend %d rows (no cost reported, not estimated)\n", unknown)
	fmt.Printf("estimated spend $%.2f over %d rows (models.dev list price, not billed)\n", estimatedTotal, estimatedRows)
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

// moveSettle is how new a flat row may be and still be skipped by the move. A
// writer of the build before the shards rewrites a row inside its live stream,
// under the flat name this move would free, and no stream outlives the stall
// timeout; an hour is past every one, and a skipped row stays flat for a later
// report, which readers read.
const moveSettle = time.Hour

// moveFlatRows is the one-time move of a pre-shard store: each flat row goes
// into the day folder of its mtime, so a windowed read stops opening it. The
// mtime, not the row's own stamp, picks the folder — the move reads no
// receipt — and no row's writer lets its file sit before its stamp's day, so
// the folder stays a safe lower bound for a windowed read. A name already
// taken in that folder (a recycled id whose other row was sharded first) is
// another row's: the flat one stays, a reader always reads the top level too,
// and never a row is destroyed. The claim is the sidecar's own — a hardlink
// under a free name — so a filesystem without hardlinks keeps its rows flat
// and still read. One report moves the store at a time: a run that finds the
// move lock held leaves the move to its holder, so a run finishing an earlier
// claim never meets the one rolling that claim back. Safe to run again, and it
// finishes what an earlier run left:
// a day-folder name that is a second name for the flat file is that run's
// claim, so the flat name goes, and a remove that fails takes the claim back,
// so no row is read from both stores (#298).
func moveFlatRows() {
	base := usageBase()
	lock, ok := moveLock(base)
	if !ok {
		return // another report is moving this store; it leaves every row in one place
	}
	defer lock.Close()
	now := time.Now()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // a day folder, or the atomic writer's in-flight tmp
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < moveSettle {
			continue // a writer before the shards may still rewrite it
		}
		dir := filepath.Join(base, info.ModTime().UTC().Format("2006-01-02"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		from := filepath.Join(base, e.Name())
		to := filepath.Join(dir, e.Name())
		if err := link(from, to); err != nil {
			// A taken name is this very row only when it is a second name for
			// the flat file: an earlier move linked it and stopped before its
			// remove. Any other file there is another row's; the flat one stays.
			if !errors.Is(err, fs.ErrExist) || !sameFile(from, to) {
				continue
			}
		}
		if err := remove(from); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// The flat name would not go: take the day name back, so the row is
			// read from one place until a later run can finish the move — only
			// while the flat name still holds this very file, never a row's
			// last name.
			if sameFile(from, to) {
				remove(to)
			}
		}
	}
}

// moveLock is the exclusive lock the move holds for its whole run. Two reports
// can run at once, and a rollback that ran beside another run's remove could
// take a row's last name; serialized, the move's steps are the only writer of
// these names. The kernel drops the lock when its process exits, so a report
// killed mid-move leaves no lock behind (#298).
func moveLock(base string) (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(base, ".move.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// sameFile is true only when both names hold one file: the claim a move left.
func sameFile(a, b string) bool {
	ia, err := os.Lstat(a)
	if err != nil {
		return false
	}
	ib, err := os.Lstat(b)
	return err == nil && os.SameFile(ia, ib)
}

// dayDirRE is a sharded day folder's name: one UTC date, spelled the way the
// writer shards a row into it.
var dayDirRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// usageRowFiles is every row file a read of the window [since, now] touches — a
// zero since reads every day folder, for a task read that has no lower bound:
// the day folders whose day falls inside the window, plus the flat rows a
// pre-shard build (or a move that could not rename) left at the top level. A
// row always sits in a folder at or after its stamp's UTC day — the writer
// places it there, and the move by an mtime no row's writer lets run before
// its stamp — so no folder before the window's first day can hold a row the
// window counts. Anything else in the store — a folder for another day, a
// name that is not a day, the atomic writer's in-flight tmp — is not read.
func usageRowFiles(base string, since, now time.Time) []string {
	first, last := since.UTC().Format("2006-01-02"), now.UTC().Format("2006-01-02")
	entries, _ := os.ReadDir(base)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			name := e.Name()
			if !dayDirRE.MatchString(name) || name < first || name > last {
				continue
			}
			sub, _ := os.ReadDir(filepath.Join(base, name))
			for _, f := range sub {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
					out = append(out, filepath.Join(base, name, f.Name()))
				}
			}
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(base, e.Name()))
		}
	}
	return out
}

// rowIdentity is the response a row records: its id, its stamp as written and,
// when it has one, its turn. It is the identity share/usage-export dedupes on,
// so a response the store holds under two names counts once. A row with no id
// has no identity and is always counted.
func rowIdentity(d *doc, ts string) (string, bool) {
	id, ok := d.str("response_id")
	if !ok || id == "" {
		return "", false
	}
	key := id + "\x00" + ts
	if turn, ok := d.str("turn_id"); ok && turn != "" {
		key += "\x00" + turn
	}
	return key, true
}

// nonFiniteToNull rewrites the NaN/Infinity/-Infinity tokens Python's json.dump
// wrote for a bad cost into null, which stays out of every bucket: such a row is
// "unknown spend", not a row to skip.
func nonFiniteToNull(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == '"' {
			e := scanString(b, i)
			out = append(out, b[i:e]...)
			i = e
			continue
		}
		matched := false
		for _, tok := range []string{"-Infinity", "Infinity", "NaN"} {
			if strings.HasPrefix(string(b[i:min(len(b), i+len(tok))]), tok) {
				out = append(out, "null"...)
				i += len(tok)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, b[i])
			i++
		}
	}
	return out
}
