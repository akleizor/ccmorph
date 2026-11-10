package ctxwindow

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// reportWidth is the width the probe's report wraps to. ccmorph-core has no
// terminal to ask, and 80 is the narrowest terminal a reader is likely to use,
// so every line fits there and on any wider one.
const reportWidth = 80

// plural is "1 model" and "3 models".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func runes(s string) int { return len([]rune(s)) }

// wrapLine writes text as lines no wider than reportWidth cells. It breaks
// only at spaces, so no word is split, and starts continuations with cont. A
// word longer than the line stays whole on a line of its own.
func wrapLine(w io.Writer, text, first, cont string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		fmt.Fprintln(w, strings.TrimRight(first, " "))
		return
	}
	line := first + words[0]
	for _, word := range words[1:] {
		if runes(line)+1+runes(word) <= reportWidth {
			line += " " + word
			continue
		}
		fmt.Fprintln(w, line)
		line = cont + word
	}
	fmt.Fprintln(w, line)
}

// ReportUnresolved says, under the profile's name, what the run looked for and
// did not find, then how to give those models a window. A model with no size
// still runs at Claude Code's default, so this is a result, not a failure.
func ReportUnresolved(w io.Writer, profile string, models []string) {
	wrapLine(w, fmt.Sprintf("%s: looked for context window sizes for %s and found none:",
		profile, plural(len(models), "model")), "", "  ")
	wrapLine(w, strings.Join(models, ", "), "  ", "  ")
	wrapLine(w, "Neither the gateway nor the model catalogue publishes a size.", "  ", "  ")
	wrapLine(w, fmt.Sprintf("The profile still works: Claude Code runs a model with no size at its %s-token default.",
		commas(CCDefaultWindow)), "  ", "  ")
	wrapLine(w, "To set a size (free):", "  ", "  ")
	// Name the one model when the whole command still fits; otherwise keep the
	// placeholder, so the line teaches the shape without wrapping mid-command.
	model := "<model>"
	if len(models) == 1 {
		free := "    ccmorph context " + profile + " " + models[0] + " <tokens>"
		paid := "    ccmorph probe " + profile + " --model " + models[0] + " --allow-inference"
		if runes(free) <= reportWidth && runes(paid) <= reportWidth {
			model = models[0]
		}
	}
	wrapLine(w, "ccmorph context "+profile+" "+model+" <tokens>", "    ", "      ")
	wrapLine(w, "To measure a size (sends one large prompt, may be billed):", "  ", "  ")
	wrapLine(w, "ccmorph probe "+profile+" --model "+model+" --allow-inference", "    ", "      ")
}

// reportProfiles prints one result group per profile, in the order the run
// tried them, each starting with the profile's name. Notes about the whole run
// (no profile) were printed as they happened and are not repeated here.
//
// sized counts the profile's models with a window. checked is nil for a run
// that saved its values (Commit already gave each profile its summary) and
// holds, per profile, how many values would change for --check, which saves
// nothing and so must say its own result.
func reportProfiles(w io.Writer, tried []string, notes []Note, sized, checked map[string]int) {
	for _, profile := range tried {
		printed := false
		var order []string
		subjects := map[string][]string{}
		for _, n := range notes {
			if n.Profile != profile || n.Reason == Unresolved {
				continue
			}
			if _, ok := subjects[n.Reason]; !ok {
				order = append(order, n.Reason)
			}
			subjects[n.Reason] = append(subjects[n.Reason], n.Subject)
		}
		for _, reason := range order {
			names := subjects[reason]
			switch {
			case len(names) > 1:
				sorted := append([]string(nil), names...)
				sort.Strings(sorted)
				shown := strings.Join(sorted[:min(4, len(sorted))], ", ")
				if len(sorted) > 4 {
					shown += fmt.Sprintf(", and %d more", len(sorted)-4)
				}
				wrapLine(w, fmt.Sprintf("%s: %s (%d): %s", profile, reason, len(sorted), shown), "", "  ")
			case names[0] == profile:
				wrapLine(w, profile+": "+reason, "", "  ")
			default:
				wrapLine(w, profile+": "+names[0]+": "+reason, "", "  ")
			}
			printed = true
		}
		var unknown []string
		seen := map[string]bool{}
		for _, n := range notes {
			if n.Profile == profile && n.Reason == Unresolved && !seen[n.Subject] {
				seen[n.Subject] = true
				unknown = append(unknown, n.Subject)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			ReportUnresolved(w, profile, unknown)
			printed = true
		}
		if printed {
			continue
		}
		if n := sized[profile]; n > 0 {
			// --check writes nothing, so its per-profile result is this line;
			// the save path has already said the same thing in Commit.
			if checked == nil {
				continue
			}
			if changed := checked[profile]; changed > 0 {
				wrapLine(w, fmt.Sprintf("%s: found context window sizes for %s; %d would change.",
					profile, plural(n, "model"), changed), "", "  ")
			} else {
				wrapLine(w, fmt.Sprintf("%s: found context window sizes for %s; no changes.",
					profile, plural(n, "model")), "", "  ")
			}
			continue
		}
		wrapLine(w, profile+": found no models to look up.", "", "  ")
	}
}
