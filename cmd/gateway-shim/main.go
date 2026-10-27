// Command gateway-shim is the stock shim: the built-in gateway compatibility
// (package compat) and no profile-specific code.
package main

import (
	"fmt"
	"os"

	"github.com/akleizor/ccmorph/compat"
	"github.com/akleizor/ccmorph/shim"
)

func main() {
	h, err := compat.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway-shim:", err)
		os.Exit(1)
	}
	shim.Main(h)
}
