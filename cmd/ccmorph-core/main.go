// Command ccmorph-core is ccmorph's compiled core: `adapter`, `shim`, `settings-compose`,
// `model-window`, `context-probe` and the `util` verbs the zsh library calls.
package main

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/compat"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/quota"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccmorph-core adapter|shim|settings-compose|model-window|context-probe|quota ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "adapter":
		adapter.Exit(adapter.Main(os.Args[2:]))
	case "tether": // internal: outlives a SIGKILLed adapter to stop its children
		adapter.Tether(os.Args[2:])
	case "settings-compose":
		os.Exit(settingscompose.Main(os.Args[2:], os.Stderr))
	case "model-window":
		os.Exit(ctxwindow.WindowMain(os.Args[2:], os.Stdout, os.Stderr))
	case "util":
		os.Exit(util.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "shim": // the stock shim; argv shifts so shim.Main sees `<port> <target>` or report|outcome
		os.Args = append([]string{os.Args[0] + " shim"}, os.Args[2:]...)
		h, err := compat.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		shim.Main(h)
	case "context-probe":
		os.Exit(ctxwindow.ProbeMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "quota":
		os.Exit(quota.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "ccmorph-core: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
