package util

import (
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// migrateProjects is `util migrate-projects <from> <to>`: move a profile-local
// sharable dir's files and links into the shared store. The profile dir is first
// renamed aside to <from>.premigration.<pid>.<n>; a name already in the store wins
// and its profile copy stays in that backup. A relative link is rewritten to an
// absolute one, resolved the way the kernel resolves it from its old place (#164,
// #167, #376, #399, #400). The caller vets the store name.
func migrateProjects(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	from, to := args[0], args[1]
	bak := from + ".premigration." + strconv.Itoa(os.Getpid()) + "." + strconv.Itoa(rand.Intn(32768))
	if os.Rename(from, bak) != nil {
		return 1
	}
	bakp := bak
	if a, err := filepath.Abs(bak); err == nil {
		if r, err := filepath.EvalSymlinks(a); err == nil {
			bakp = r
		}
	}
	_ = filepath.WalkDir(bak, func(f string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if n := d.Name(); n == ".DS_Store" || n == ".localized" {
			return nil
		}
		rel := strings.TrimPrefix(f, bak+"/")
		dest := to + "/" + rel
		if _, err := os.Lstat(dest); err == nil {
			return nil
		}
		if os.MkdirAll(zshH(dest), 0o777) != nil {
			return nil
		}
		tgt := ""
		if d.Type()&fs.ModeSymlink != 0 {
			tgt, _ = os.Readlink(f)
		}
		if tgt == "" || strings.HasPrefix(tgt, "/") {
			// shortcut: a plain rename, so a store on another filesystem keeps the file in the backup; copy when profiles span volumes.
			_ = os.Rename(f, dest)
			return nil
		}
		// The parent resolves physically (a link's `..` leaves the link target); the
		// leaf stays unresolved so a link to a sibling link follows that sibling.
		phys, leaf := bakp+"/"+zshH(rel)+"/"+tgt, ""
		if !(tgt == "." || tgt == ".." || strings.HasSuffix(tgt, "/.") || strings.HasSuffix(tgt, "/..")) {
			leaf, phys = "/"+zshT(phys), zshH(phys)
		}
		// A link dir moved earlier is retried in the store; a parent that resolves
		// nowhere keeps its unresolvable tail as spelled, so the link dangles where it
		// pointed rather than collapsing onto another file.
		pdir, ok := physDir(phys)
		if !ok {
			pdir, ok = physDir(to + "/" + strings.TrimPrefix(phys, bakp+"/"))
		}
		if !ok {
			pdir, ok = physPrefix(phys)
		}
		if !ok {
			return nil
		}
		phys = strings.TrimSuffix(pdir, "/") + leaf
		if strings.HasPrefix(phys, bakp+"/") {
			phys = to + "/" + strings.TrimPrefix(phys, bakp+"/")
		}
		if os.Symlink(phys, dest) == nil {
			_ = os.Remove(f)
		}
		return nil
	})
	return 0
}

// physDir is a directory's physical path plus a trailing "/" (so the root is "//"),
// the way a `cd -P` reports it; false when it is not a reachable directory.
func physDir(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		p = wd + "/" + p
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
		return "", false
	}
	if _, err := os.ReadDir(r); err != nil {
		return "", false
	}
	return r + "/", true
}

// physPrefix resolves p's longest existing prefix and keeps the rest as spelled.
func physPrefix(p string) (string, bool) {
	head, rest := p, ""
	dir, ok := physDir(head)
	for !ok {
		if head == zshH(head) {
			return "", false
		}
		rest = "/" + zshT(head) + rest
		head = zshH(head)
		dir, ok = physDir(head)
	}
	if dir == "//" {
		dir = ""
	}
	if rest == "" {
		rest = "/"
	}
	return strings.TrimSuffix(dir, "/") + rest + "/", true
}

// zshH and zshT are zsh's :h and :t modifiers: no lexical cleaning, so `x/..`
// keeps its `..`.
func zshH(p string) string {
	if p == "" {
		return "."
	}
	q := strings.TrimRight(p, "/")
	if q == "" {
		return "/"
	}
	i := strings.LastIndex(q, "/")
	if i < 0 {
		return "."
	}
	if h := strings.TrimRight(q[:i], "/"); h != "" {
		return h
	}
	return "/"
}

func zshT(p string) string {
	q := strings.TrimRight(p, "/")
	return q[strings.LastIndex(q, "/")+1:]
}
