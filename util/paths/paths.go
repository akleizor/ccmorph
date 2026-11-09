// Package paths holds the installed layout's durable contracts: where
// machine-local state lives and where the driver script lives. Each was a
// small helper hand-written in every package that needed it, and copies drift
// (#303); each has this one definition instead. The zsh layer resolves the
// same paths in lib/ccmorph.sh and bin/ccmorph.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// StateDir is the XDG state root, where usage rows, receipts, prices and the
// mask key live: $XDG_STATE_HOME/ccmorph, else ~/.local/state/ccmorph. It is
// derived state (docs/ARCHITECTURE.md): deleting it loses no configuration,
// no profile and no credential.
func StateDir() string {
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return filepath.Join(root, "ccmorph")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccmorph")
}

// LibDir is the installation's lib/ directory: $CCMORPH_LIB_DIR, else the
// lib/ beside the running binary (lib/ finds libexec/ and share/ beside
// itself).
func LibDir() string {
	if v := os.Getenv("CCMORPH_LIB_DIR"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib")
}

// Script is the driver script, the runtime's only entry point: lib/ccmorph.sh.
func Script() string { return filepath.Join(LibDir(), "ccmorph.sh") }
