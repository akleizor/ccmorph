// Package paths holds the installed layout's durable contracts: where
// machine-local state lives, where profiles and the registry live, where the
// driver script lives, the file predicates and the symlink resolver every
// package asks of a path, and the atomic write every state file goes through.
// Each was a small helper hand-written in every package that needed it, and
// copies drift (#303, #422, #521, #523, #520); each has this one definition
// instead. The zsh layer resolves the same state and script paths in
// lib/ccmorph.sh and bin/ccmorph.
package paths

import (
	"fmt"
	"os"
	"os/user"
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

// AccountHome is the passwd entry's home, for a process started with HOME
// unset. A variable so a test can pin the account the way a real one cannot.
var AccountHome = func() string {
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// Home is the home directory the profile root is built on: $HOME, else the
// account's own from the password database — and when it had to look, the
// answer is re-exported as HOME for every later reader (#138: canonical
// settings, the projects stores, doctor, rm, setup-profile, settings-compose
// and Claude Code itself all read HOME), never a path relative to the cwd.
// With neither HOME nor an account home nothing absolute can be built on, so
// the process stops there with the one reason and the fix, as the launcher
// always has.
func Home() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	home := AccountHome()
	if home == "" {
		fmt.Fprintln(os.Stderr, "ccmorph: HOME is unset and no home directory is known; set HOME")
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	return home
}

// ProfileRoot is where profiles and the registry live, the ladder every
// builder of that root answers: $CCMORPH_STATE_DIR, else
// $XDG_CONFIG_HOME/ccmorph, else ~/.config/ccmorph over Home — the ladder
// lib/ccmorph.sh spells as
// _CCMORPH_DIR="${CCMORPH_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph}".
// bin/ccmorph keeps a deliberately-parallel derivation of its own: it must
// find lib/ccmorph.sh from zsh before any Go code can run. The three Go
// spellings this replaced (#523) agreed only under a launch, and answered
// two different nonexistent roots when the core ran on its own with HOME
// unset.
func ProfileRoot() string {
	if v := os.Getenv("CCMORPH_STATE_DIR"); v != "" {
		return filepath.Clean(v)
	}
	if cfg := os.Getenv("XDG_CONFIG_HOME"); cfg != "" {
		return filepath.Join(cfg, "ccmorph")
	}
	return filepath.Join(Home(), ".config", "ccmorph")
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

// Exists reports whether path can be stat'ed at all.
func Exists(path string) bool { _, err := os.Stat(path); return err == nil }

// IsDir reports whether path is a directory.
func IsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// IsFile reports whether path is a regular file.
func IsFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// IsSymlink reports whether path itself is a symbolic link (a link to a
// directory is a symlink, not a directory).
func IsSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// Lexists reports whether path itself can be lstat'ed: a broken symlink
// lexists, where Exists follows the link and answers no.
func Lexists(path string) bool { _, err := os.Lstat(path); return err == nil }

// Resolve is p with every symlink along it evaluated — Path.resolve() with a
// missing tail kept: a path EvalSymlinks cannot follow (nothing is named, or
// a link loops) resolves to its absolute form instead, and an empty p stays
// empty, the callers' marker for an unset path.
func Resolve(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
