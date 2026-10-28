// Command ccmorph-core is ccmorph's compiled core; its `adapter` subcommand is
// the Go port of libexec/ccmorph-adapter.
package main

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/adapter"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccmorph-core adapter <profile> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "adapter":
		adapter.Exit(adapter.Main(os.Args[2:]))
	case "tether": // internal: outlives a SIGKILLed adapter to stop its children
		adapter.Tether(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "ccmorph-core: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
