package cli

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/akleizor/ccmorph/util/paths"
)

// brewOptCore is the core behind Homebrew's stable opt link when libexec,
// resolved, sits in a Homebrew keg (<prefix>/Cellar/ccmorph/<version>/...) and
// <prefix>/opt/ccmorph reaches a core there; "" anywhere else (a clone,
// install.sh's layout, a Cellar-shaped path without brew's link). brew
// re-points opt at every upgrade, so a link naming this survives `brew upgrade`
// and `brew cleanup` (#287, #674).
func brewOptCore(libexec string) string {
	if libexec == "" {
		return ""
	}
	m := brewCellarRE.FindStringSubmatch(paths.Resolve(libexec))
	if m == nil {
		return ""
	}
	core := filepath.Join(filepath.Clean(m[1]+"/opt/ccmorph"+m[2]), "ccmorph-core")
	if !paths.IsFile(core) {
		return ""
	}
	return core
}

// configCore is the core's path in install.sh's layout,
// ${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph/libexec/ccmorph-core: the path
// tools outside ccmorph resolve it by (a hive landing's settle() records its
// outcome through it). It is spelled from XDG_CONFIG_HOME, never
// CCMORPH_STATE_DIR, because install.sh places code there either way.
func configCore() string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(paths.Home(), ".config")
	}
	return filepath.Join(cfg, "ccmorph", "libexec", "ccmorph-core")
}

// publishCore gives a Homebrew install the core path install.sh's layout has
// (#674). Homebrew keeps the core at <keg>/libexec/libexec/ccmorph-core and
// cannot write $HOME, so a tool that resolves configCore found nothing on a
// Homebrew-only machine. Each stateful command links configCore to
// brewOptCore. It writes only inside ccmorph's own config dir, only once that
// dir exists, and never replaces a file or a working link already there (an
// install.sh core, a user's own link); a dangling link is repaired. Best
// effort: a failure leaves the command's own work alone.
func (l *launcher) publishCore() {
	target := brewOptCore(l.libexec)
	if target == "" {
		return
	}
	dest := configCore()
	root := filepath.Dir(filepath.Dir(dest))
	if !paths.IsDir(root) || paths.IsSymlink(root) {
		return
	}
	if paths.Lexists(dest) && (!paths.IsSymlink(dest) || paths.Exists(dest)) {
		return
	}
	if os.MkdirAll(filepath.Dir(dest), 0o700) != nil {
		return
	}
	// Made beside dest and renamed over it, so a reader never sees a half-made
	// path and two commands racing both leave the same link.
	tmp := dest + ".tmp-" + strconv.Itoa(os.Getpid())
	os.Remove(tmp)
	if os.Symlink(target, tmp) != nil {
		return
	}
	if os.Rename(tmp, dest) != nil {
		os.Remove(tmp)
	}
}
