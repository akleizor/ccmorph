package util

import (
	"errors"
	"strings"
)

// LaunchedArgv is run's own first walk of its argv.
type LaunchedArgv struct {
	Profile     string
	Interactive bool
	Forward     []string
}

// ParseLaunchArgv claims -a/--profile/--profile=/-i/--interactive and forwards
// every other token, stepping over a native option's value (arity table: spelling
// to flag, value or optional) so a value that looks like one of these flags stays
// Claude Code's (#405). A misuse is an error carrying the message.
func ParseLaunchArgv(args []string, arity map[string]string) (LaunchedArgv, error) {
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
	fail := errors.New
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
				return LaunchedArgv{}, fail(a + " requires an profile name")
			}
			if seen {
				return LaunchedArgv{}, fail(a + " may be specified only once")
			}
			profile, seen = args[i+1], true
			i++
		case strings.HasPrefix(a, "--profile="):
			if seen {
				return LaunchedArgv{}, fail("--profile= may be specified only once")
			}
			if profile = strings.TrimPrefix(a, "--profile="); profile == "" {
				return LaunchedArgv{}, fail("--profile= requires an profile name")
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
	return LaunchedArgv{profile, interactive, fwd}, nil
}
