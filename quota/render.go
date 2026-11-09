package quota

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// maxWidth is a phone-width terminal: 60 columns, so no line wraps.
const maxWidth = 60

// barCells is how much of the width a bar takes; 16 leaves room for the label,
// the percent and a reset time in 60 columns.
const barCells = 16

var groups = []struct{ kind, title string }{
	{"claude", "Claude accounts and tokens"},
	{"codex", "Codex subscriptions"},
	{"budget", "Budget accounts"},
	{"gateway", "API key and provider profiles"},
}

// renderText prints the whole view: one group per kind, one entry per profile,
// header line then one bar line per window. light is the #274 light palette:
// the same styles, with dim a fixed grey instead of SGR 2.
func renderText(w io.Writer, entries []Entry, color, light bool) {
	first := true
	for _, g := range groups {
		rows := []Entry{}
		for _, e := range entries {
			if e.Kind == g.kind {
				rows = append(rows, e)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if !first {
			fmt.Fprintln(w)
		}
		first = false
		emit(w, g.title, color, light, "bold")
		for _, e := range rows {
			renderEntry(w, e, color, light)
		}
	}
}

func renderEntry(w io.Writer, e Entry, color, light bool) {
	style := ""
	if e.Stale {
		style = "dim"
	}
	head := e.Profile
	if e.Plan != "" {
		head += "  " + e.Plan
	}
	reason := e.Error
	if e.Stale {
		if e.AgeSeconds != nil {
			head += "  " + ageText(*e.AgeSeconds) + ", " + reason
			reason = ""
		}
	}
	emit(w, head, color, light, style)
	for _, win := range e.Windows {
		emit(w, windowLine(win), color, light, style)
	}
	if e.Spend != nil {
		emit(w, spendLine(e.Spend), color, light, style)
		if l := unpricedLine(e.Spend); l != "" {
			emit(w, l, color, light, style)
		}
	}
	if e.Balance != "" {
		emit(w, "  balance "+e.Balance, color, light, style)
	}
	if reason != "" {
		emit(w, "  "+reason, color, light, style)
	}
}

// windowLine is "  5h       ██░░░░░░░░░░░░░░  12%  in 4h 10m".
func windowLine(win Window) string {
	return fmt.Sprintf("  %-8s %s %4s  %s", win.Label, bar(win.Used), percent(win.Used), resetText(win.ResetsAt))
}

// spendLine is a gateway or budget profile's use. A price turns into money;
// without one the turns and tokens still show and the cost says it is unknown,
// never $0.00; no receipts at all say the profile was not used.
func spendLine(s *Spend) string {
	if s.MonthTurns == 0 {
		return "  not used this month"
	}
	if s.UnpricedTurns == s.MonthTurns {
		return fmt.Sprintf("  %d %s · %d %s this month · cost unknown",
			s.MonthTurns, plural(int64(s.MonthTurns), "turn"), s.MonthTokens, plural(s.MonthTokens, "token"))
	}
	line := fmt.Sprintf("  today $%.2f · month $%.2f", s.TodayUSD, s.MonthUSD)
	if s.BudgetUSD != nil {
		line += " of $" + trimFloat(*s.BudgetUSD)
	}
	if s.Estimated {
		line += " (est.)"
	}
	return line
}

// unpricedLine is the mixed case: some of the month's turns were priced and some
// were not. It sits on its own line so the money line stays within the width and
// the note is never truncated away. Empty otherwise.
func unpricedLine(s *Spend) string {
	if s.UnpricedTurns == 0 || s.UnpricedTurns >= s.MonthTurns {
		return ""
	}
	return fmt.Sprintf("  %d %s with no price · cost unknown", s.UnpricedTurns, plural(int64(s.UnpricedTurns), "turn"))
}

// plural is the word for n of it: one turn, two turns.
func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// bar is barCells cells: rounded, clamped, so a spent window is a full bar.
func bar(used float64) string {
	filled := int(math.Round(used * barCells))
	if filled < 0 {
		filled = 0
	}
	if filled > barCells {
		filled = barCells
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", barCells-filled)
}

func percent(used float64) string { return fmt.Sprintf("%.0f%%", used*100) }

// resetText is when a window resets, in local time: "in 2h 14m" under a day,
// "Tue 09:00" under a week, a date beyond that.
func resetText(at *int64) string {
	if at == nil {
		return ""
	}
	t := time.Unix(*at, 0)
	d := t.Sub(now())
	switch {
	case d <= 0:
		return "reset"
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if h == 0 {
			return fmt.Sprintf("in %dm", m)
		}
		if m == 0 {
			return fmt.Sprintf("in %dh", h)
		}
		return fmt.Sprintf("in %dh %dm", h, m)
	case d < 7*24*time.Hour:
		return t.Local().Format("Mon 15:04")
	}
	return t.Local().Format("Jan 2")
}

// emit writes one line: truncated to maxWidth runes so it never wraps, then
// styled only when the terminal supports colour. On a light background dim is
// a fixed grey, the same #6b6b6b the cli package paints with (#274), because a
// terminal may render SGR 2 below any readable contrast there.
func emit(w io.Writer, s string, color, light bool, style string) {
	r := []rune(s)
	if len(r) > maxWidth {
		s = string(r[:maxWidth-1]) + "…"
	}
	switch {
	case !color:
	case style == "bold":
		s = "\033[1m" + s + "\033[0m"
	case style == "dim":
		pre := "\033[2m"
		if light {
			pre = "\033[38;2;107;107;107m"
		}
		s = pre + s + "\033[0m"
	}
	fmt.Fprintln(w, s)
}
