// Command gateway-shim is the stock shim: no per-profile hooks.
package main

import "github.com/akleizor/ccmorph/shim"

func main() { shim.Main(shim.Hooks{}) }
