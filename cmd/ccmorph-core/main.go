// Command ccmorph-core is ccmorph's compiled core. Its subcommands are the Go
// ports of the Python helpers: `adapter` (libexec/ccmorph-adapter),
// `settings-compose`, `model-window` and `refresh-model-registry`.
package main

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/settingscompose"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccmorph-core adapter|settings-compose|model-window|refresh-model-registry ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "adapter":
		adapter.Exit(adapter.Main(os.Args[2:]))
	case "tether": // internal: outlives a SIGKILLed adapter to stop its children
		adapter.Tether(os.Args[2:])
	case "settings-compose":
		os.Exit(settingscompose.Main(os.Args[2:], os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "ccmorph-core: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
