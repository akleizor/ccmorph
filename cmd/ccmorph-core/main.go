// Command ccmorph-core is ccmorph's compiled core: one binary whose first word
// picks the command, from the table below.
package main

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/cli"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/quirks"
	"github.com/akleizor/ccmorph/quota"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util"
)

// commands is every first word the core answers to; the usage line is built
// from it, so a new command is listed once. Each runs on the words after its
// name and does not return.
var commands = map[string]func(args []string){
	"adapter": func(a []string) { adapter.Exit(adapter.Main(a)) },
	// the Remote Control edge's proxy (addons/remote-control/ccmorph-rc starts it)
	"edge-proxy": func(a []string) { os.Exit(agentproxy.EdgeMain(a, os.Stdout, os.Stderr)) },
	// internal: outlives a SIGKILLed adapter to stop its children
	"tether":           func(a []string) { adapter.Tether(a) },
	"settings-compose": func(a []string) { os.Exit(settingscompose.Main(a, os.Stderr)) },
	"model-window":     func(a []string) { os.Exit(ctxwindow.WindowMain(a, os.Stdout, os.Stderr)) },
	// `ccmorph run`: the launch and relaunch loop
	"launch": func(a []string) { os.Exit(cli.Main(a)) },
	// the `ccmorph` verbs that run in the core
	"cli":  func(a []string) { os.Exit(cli.Verb(a)) },
	"util": func(a []string) { os.Exit(util.Main(a, os.Stdin, os.Stdout, os.Stderr)) },
	// the Stop hook: rows from the client's own transcript
	"usage-row":     func(a []string) { os.Exit(shim.UsageRowMain(a, os.Stdin)) },
	"usage-compact": func(a []string) { os.Exit(shim.UsageCompactMain(a, os.Stdout, os.Stderr)) },
	// the stock shim; argv shifts so shim.Main sees `<port> <target>` or report|outcome
	"shim": func(a []string) {
		os.Args = append([]string{os.Args[0] + " shim"}, a...)
		h, err := quirks.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		shim.Main(h)
	},
	"context-probe": func(a []string) { os.Exit(ctxwindow.ProbeMain(a, os.Stdin, os.Stdout, os.Stderr)) },
	"quota":         func(a []string) { os.Exit(quota.Main(a, os.Stdin, os.Stdout, os.Stderr)) },
}

func usage() string {
	return "usage: ccmorph-core " + strings.Join(slices.Sorted(maps.Keys(commands)), "|") + " ..."
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "ccmorph-core: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	run(os.Args[2:])
}
