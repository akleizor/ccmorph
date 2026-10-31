package util

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// launchArgv is `util launch-argv <args...>`: run's own first walk. It claims
// -a/--profile/--profile=/-i/--interactive and forwards every other token,
// stepping over a native option's value (arity table on stdin, `opt arity` lines)
// so a value that looks like one of these flags stays Claude Code's (#405).
// Prints `profile=<p>`, `interactive=<0|1>`, then the forwarded args NUL-joined
// with a trailing ".". Exit 2 with the message on stderr for a misuse.
func launchArgv(args []string, in io.Reader, out, errw io.Writer) int {
	arity := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok {
			arity[k] = v
		}
	}
	claims := func(a string) bool {
		return a == "-a" || a == "--profile" || strings.HasPrefix(a, "--profile=") || a == "-i" || a == "--interactive"
	}
	flagAhead := func(rest []string) bool {
		for _, a := range rest {
			if a == "--" {
				return false
			}
			if claims(a) {
				return true
			}
		}
		return false
	}
	fail := func(msg string) int { fmt.Fprintln(errw, msg); return 2 }
	profile, seen, interactive := "", false, false
	var fwd []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			fwd = append(fwd, args[i:]...)
			i = len(args)
		case a == "-a" || a == "--profile":
			if i+1 >= len(args) || args[i+1] == "" || args[i+1] == "--" {
				return fail(a + " requires an profile name")
			}
			if seen {
				return fail(a + " may be specified only once")
			}
			profile, seen = args[i+1], true
			i++
		case strings.HasPrefix(a, "--profile="):
			if seen {
				return fail("--profile= may be specified only once")
			}
			if profile = strings.TrimPrefix(a, "--profile="); profile == "" {
				return fail("--profile= requires an profile name")
			}
			seen = true
		case a == "-i" || a == "--interactive":
			interactive = true
		case len(a) > 1 && a[0] == '-':
			owns := 1
			if flagAhead(args[i+1:]) && !strings.Contains(a, "=") && i+1 < len(args) {
				switch arity[a] {
				case "value":
					owns = 2
				case "optional":
					if n := args[i+1]; n != "" && n[0] != '-' {
						owns = 2
					}
				}
			}
			fwd = append(fwd, args[i:i+owns]...)
			i += owns - 1
		default:
			fwd = append(fwd, a)
		}
	}
	in01 := "0"
	if interactive {
		in01 = "1"
	}
	fmt.Fprintf(out, "profile=%s\ninteractive=%s\n", profile, in01)
	for _, a := range fwd {
		io.WriteString(out, a+"\x00")
	}
	io.WriteString(out, ".")
	return 0
}
