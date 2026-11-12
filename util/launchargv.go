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
// every other token with the launch flag grammar (stepLaunch): an option steps
// over the value it owns — its own spellings by their rule, the rest by the
// arity table — so a value that looks like one of these flags stays Claude
// Code's (#405). A misuse is an error carrying the message.
func ParseLaunchArgv(args []string, arity map[string]string) (LaunchedArgv, error) {
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
		default:
			s := stepLaunch(args, arity, i)
			fwd = append(fwd, args[i:i+s.wide]...)
			i += s.wide - 1
		}
	}
	return LaunchedArgv{profile, interactive, fwd}, nil
}
