// Command ccmorph-core is ccmorph's compiled core: `adapter`, `settings-compose`,
// `model-window`, `context-probe` and the `util` verbs the zsh library calls.
package main

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/contextprobe"
	"github.com/akleizor/ccmorph/modelwindow"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/util"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccmorph-core adapter|settings-compose|model-window|context-probe ...")
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
		os.Exit(modelwindow.Main(os.Args[2:], os.Stdout, os.Stderr))
	case "util":
		os.Exit(util.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "context-probe":
		os.Exit(contextprobe.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "ccmorph-core: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
