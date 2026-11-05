package util

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// migrateProjects moves a profile-local sharable dir's files and links into the
// shared store by copying (#177): the store may sit on another filesystem,
// where a rename cannot reach, so every regular file is written to a
// destination-local temp file, fsynced, verified by size and hash, and linked
// into place only where no entry exists yet. A same-name entry the store
// already holds wins unless it is a carried link giving way or a byte-identical
// duplicate; a divergent one — or a link the store cannot take — is kept side by side in
// '<from>.premigration.<pid>.<n>' (a link stays a link) and named in report. The originals stay where they are until every file has
// verified, so a failure anywhere leaves the profile's own copy whole — the
// caller then keeps the profile on it and the launch still succeeds. A relative
// link is rewritten to an absolute one, resolved the way the kernel resolves it
// from its old place (#164, #167, #376, #399, #400). A `from` that is itself a
// link names the user's own store, on another disk: its target is read, never
// written, and left intact; only the link is replaced. The caller vets the
// store name.
func migrateProjects(from, to string, report func(string)) bool {
	if report == nil {
		report = func(string) {}
	}
	walk, ownStore := from, false
	if fi, err := os.Lstat(from); err != nil {
		return false
	} else if fi.Mode()&fs.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(from)
		if err != nil {
			return false
		}
		walk, ownStore = r, true
	}
	// The tree's physical root, the way a link's `..` resolves through it.
	root := walk
	if r, err := filepath.EvalSymlinks(walk); err == nil {
		root = r
	}
	var remove []walked
	var diverge []walked
	ok := true
	_ = filepath.WalkDir(walk, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			ok = false // an unreadable entry is never moved blindly
			return nil
		}
		if d.IsDir() {
			if f == walk {
				return nil
			}
			// #185: a carried sidecar dir arrives as a link at the store path.
			// It must give way before its children are walked: resolving through
			// the link would make every file look like a duplicate of itself.
			dest := to + "/" + strings.TrimPrefix(f, walk+"/")
			if isLink(dest) && linkGivesWay(dest, f) {
				if os.Remove(dest) != nil {
					ok = false
					return fs.SkipDir
				}
			}
			return nil
		}
		if n := d.Name(); n == ".DS_Store" || n == ".localized" {
			// Finder junk must not hold the tree open; on the user's own disk
			// it stays (#177: the target is left intact).
			if !ownStore {
				_ = os.Remove(f)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			ok = false // no stat means nothing can be verified as unchanged later
			return nil
		}
		rel := strings.TrimPrefix(f, walk+"/")
		dest := to + "/" + rel
		if lexists(dest) {
			switch {
			case d.Type()&fs.ModeSymlink != 0 && sameTarget(f, dest):
				// The profile's link already reaches this very entry in the store:
				// only the link goes. Writing it at dest would loop onto itself.
				tgt, _ := os.Readlink(f)
				remove = append(remove, walked{f, fi, tgt})
				return nil
			case linkGivesWay(dest, f):
				// A carried session link gives way to the transcript it points
				// at (or to anything, once it dangles), so the real file never
				// stays behind (#173).
				if os.Remove(dest) != nil {
					ok = false
					return nil
				}
			case d.Type()&fs.ModeSymlink != 0:
				// A link never overwrites a settled entry.
				diverge = append(diverge, atWalk(f, fi))
				return nil
			case resolved(dest) == resolved(f):
				// #185: the store path reaches this very file through a link on a
				// parent dir, so it is never a removable duplicate.
				ok = false
				return nil
			case sameContent(dest, f):
				// The store already holds this very content; the profile's
				// copy is a duplicate that can go once everything verifies.
				remove = append(remove, walked{f, fi, ""})
				return nil
			default:
				diverge = append(diverge, atWalk(f, fi))
				return nil
			}
		}
		if os.MkdirAll(zshH(dest), 0o777) != nil {
			ok = false
			return nil
		}
		tgt := ""
		if d.Type()&fs.ModeSymlink != 0 {
			tgt, _ = os.Readlink(f)
		}
		if tgt == "" || strings.HasPrefix(tgt, "/") {
			// A regular file is copied and verified; an absolute link moves as
			// a link with the target it already spells.
			if tgt == "" {
				if !d.Type().IsRegular() {
					ok = false // a socket or device is never moved blindly
					return nil
				}
				if copyVerified(f, dest) != nil {
					ok = false
					return nil
				}
			} else if resolved(tgt) == resolved(dest) {
				// #185: the target was pruned from the store; writing it here
				// would make a link to itself. Park it beside the profile.
				diverge = append(diverge, atWalk(f, fi))
				return nil
			} else if os.Symlink(tgt, dest) != nil {
				diverge = append(diverge, atWalk(f, fi)) // parked as a link, the merge goes on
				return nil
			}
			remove = append(remove, walked{f, fi, tgt})
			return nil
		}
		// The parent resolves physically (a link's `..` leaves the link target); the
		// leaf stays unresolved so a link to a sibling link follows that sibling.
		phys, leaf := root+"/"+zshH(rel)+"/"+tgt, ""
		if !(tgt == "." || tgt == ".." || strings.HasSuffix(tgt, "/.") || strings.HasSuffix(tgt, "/..")) {
			leaf, phys = "/"+zshT(phys), zshH(phys)
		}
		// A link dir moved earlier is retried in the store; a parent that resolves
		// nowhere keeps its unresolvable tail as spelled, so the link dangles where it
		// pointed rather than collapsing onto another file.
		pdir, found := physDir(phys)
		if !found {
			pdir, found = physDir(to + "/" + strings.TrimPrefix(phys, root+"/"))
		}
		if !found {
			pdir, found = physPrefix(phys)
		}
		if !found {
			diverge = append(diverge, atWalk(f, fi))
			return nil
		}
		phys = strings.TrimSuffix(pdir, "/") + leaf
		if strings.HasPrefix(phys, root+"/") {
			phys = to + "/" + strings.TrimPrefix(phys, root+"/")
		}
		if os.Symlink(phys, dest) == nil {
			remove = append(remove, walked{f, fi, tgt})
		} else {
			diverge = append(diverge, atWalk(f, fi)) // parked as a link, the merge goes on
		}
		return nil
	})
	if !ok {
		return false
	}
	if len(diverge) > 0 {
		bak := from + ".premigration." + strconv.Itoa(os.Getpid()) + "." + strconv.Itoa(rand.Intn(32768))
		for _, w := range diverge {
			f := w.f
			rel := strings.TrimPrefix(f, walk+"/")
			if li, err := os.Lstat(f); err == nil && li.Mode()&fs.ModeSymlink != 0 {
				// A link is parked as the link it is, its literal target kept;
				// copyVerified would follow it.
				lt, err := os.Readlink(f)
				if err != nil || os.MkdirAll(filepath.Dir(bak+"/"+rel), 0o777) != nil || os.Symlink(lt, bak+"/"+rel) != nil {
					return false
				}
			} else if copyVerified(f, bak+"/"+rel) != nil {
				return false
			}
			report("kept '" + rel + "' in '" + bak + "': it could not join the shared store as is")
			// The parked copy is verified; the original can go with the rest —
			// but only as the walk saw it: a line that arrived while the park
			// copy ran keeps the original for the next launch (#177, #185).
			remove = append(remove, w)
		}
	}
	// The originals go only now that every file has verified, and only where
	// nothing touched them since: a file a live session appended to, or a link
	// that was replaced, stays for the next launch rather than losing what
	// arrived after the copy (#177). The user's own store on another disk
	// keeps every file.
	for _, w := range remove {
		if !ownStore && stillUnchanged(w) {
			_ = os.Remove(w.f) // a refusal keeps the dir, and prune reports it
		}
	}
	if ownStore {
		return os.Remove(from) == nil
	}
	// Only dirs that are now empty go: anything left — a file that landed
	// after the walk — keeps its dir, and the caller keeps the profile on
	// that dir until a later launch migrates what arrived (#177).
	return pruneEmpty(from)
}

// atWalk is a divergent entry with the identity it had when the walk saw it:
// the park copy runs later, and a file that changed in between must keep its
// original (#185).
func atWalk(f string, fi os.FileInfo) walked {
	w := walked{f: f, fi: fi}
	if fi != nil && fi.Mode()&fs.ModeSymlink != 0 {
		w.fi = nil // a link's identity is its literal target
		w.link, _ = os.Readlink(f)
	}
	return w
}

// walked is one migrated entry with the identity it had when the walk saw it:
// what the removal step must re-check before the original goes (#177).
type walked struct {
	f    string
	fi   os.FileInfo // walk-time size and mtime of a regular file
	link string      // the textual target a symlink was migrated as
}

// stillUnchanged reports whether the entry is what the walk saw: a regular
// file of the same size and mtime, or a link with the same target. Anything a
// live session rewrote or replaced since the copy stays for the next launch
// (#177).
func stillUnchanged(w walked) bool {
	fi, err := os.Lstat(w.f)
	if err != nil {
		return false
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(w.f)
		return err == nil && t == w.link
	}
	if w.fi == nil {
		return false // the walk never saw its size and mtime: never remove
	}
	return fi.Size() == w.fi.Size() && fi.ModTime().Equal(w.fi.ModTime())
}

// pruneEmpty removes a tree's directories bottom-up, only where they are
// empty, and reports whether the whole tree went: anything still inside — a
// file that arrived after the walk — survives with its dir (#177).
func pruneEmpty(root string) bool {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // a non-empty dir refuses, keeping what is inside
	}
	_, err := os.Lstat(root)
	return err != nil
}

// linkFile publishes a verified temp file; a test seam for the filesystems
// that refuse hard links (#185).
var linkFile = os.Link

// copyVerified copies the regular file f into dest through a destination-local
// temp file (#177): the store may sit on another filesystem, where a rename
// cannot reach. The bytes are hashed as they stream, the temp file is synced,
// and both size and hash are read back and compared before the file is linked
// into place — an existing dest is never overwritten.
func copyVerified(f, dest string) error {
	src, err := os.Open(f)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ccm-copy-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := os.Chmod(name, fi.Mode().Perm()); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	h := sha256.New()
	_, werr := io.Copy(io.MultiWriter(tmp, h), src)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(name)
		return werr
	}
	// Read the temp back: the hash proves what landed on the store's own
	// filesystem, not what was sent to it.
	got, err := fileHash(name)
	if err != nil {
		os.Remove(name)
		return err
	}
	if got != [sha256.Size]byte(h.Sum(nil)) {
		os.Remove(name)
		return errors.New(f + ": the copied bytes did not verify")
	}
	if fi2, err := os.Stat(name); err != nil || fi2.Size() != fi.Size() {
		os.Remove(name)
		return errors.New(f + ": the copied size did not verify")
	}
	// A migrated session keeps its age: /resume sorts by modification time.
	if err := os.Chtimes(name, fi.ModTime(), fi.ModTime()); err != nil {
		os.Remove(name)
		return err
	}
	// Publish without overwrite: link(2) refuses an existing dest, so a file
	// another writer created meanwhile is never clobbered.
	err = linkFile(name, dest)
	if err == nil || errors.Is(err, fs.ErrExist) {
		os.Remove(name)
		return err
	}
	// #185: exFAT, FAT and some SMB mounts refuse hard links; rename is atomic
	// too (shim/mask.go's key falls back the same way). The reserved name keeps
	// a file another writer created meanwhile from being clobbered.
	reserved, err2 := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err2 != nil {
		os.Remove(name)
		return err2
	}
	reserved.Close()
	if err2 = os.Rename(name, dest); err2 != nil {
		os.Remove(name)
		os.Remove(dest)
		return err2
	}
	return nil
}

// linkGivesWay reports whether the link at dest names f or names nothing.
func linkGivesWay(dest, f string) bool {
	if !isLink(dest) {
		return false // a real file in the store never gives way
	}
	t, err := os.Stat(dest)
	if err != nil {
		return true
	}
	o, err := os.Stat(f)
	return err == nil && os.SameFile(t, o)
}

// sameTarget reports whether both paths resolve to one existing file.
func sameTarget(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

// sameContent reports whether two paths hold the same regular content.
func sameContent(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil || !sa.Mode().IsRegular() {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil || !sb.Mode().IsRegular() || sa.Size() != sb.Size() {
		return false
	}
	if os.SameFile(sa, sb) {
		return true
	}
	ha, err := fileHash(a)
	if err != nil {
		return false
	}
	hb, err := fileHash(b)
	return err == nil && ha == hb
}

func fileHash(p string) ([sha256.Size]byte, error) {
	var h [sha256.Size]byte
	f, err := os.Open(p)
	if err != nil {
		return h, err
	}
	defer f.Close()
	g := sha256.New()
	if _, err := io.Copy(g, f); err != nil {
		return h, err
	}
	copy(h[:], g.Sum(nil))
	return h, nil
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

// mergeParks returns the transcripts parked beside a profile to /resume: a
// v0.5.0 launch and every earlier failed merge left '<what>.premigration.*'
// dirs holding files that exist nowhere else. Each parked file the shared
// store does not already hold is copied in and leaves the park; a file the
// store holds differently stays in its park and is named, never clobbered. An
// emptied park goes (#185).
func mergeParks(dir, store string, report func(string)) {
	if report == nil {
		report = func(string) {}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "projects.premigration.") {
			continue
		}
		park := filepath.Join(dir, e.Name())
		kept := 0
		_ = filepath.WalkDir(park, func(f string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel := strings.TrimPrefix(f, park+"/")
			dest := store + "/" + rel
			if lexists(dest) {
				if sameContent(dest, f) {
					_ = os.Remove(f)
					return nil
				}
				report("kept '" + rel + "' in '" + park + "': the shared store already holds a different copy")
				kept++
				return nil
			}
			if os.MkdirAll(zshH(dest), 0o777) != nil {
				kept++
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				if t, err := os.Readlink(f); err != nil || os.Symlink(t, dest) != nil {
					kept++
					return nil
				}
				_ = os.Remove(f)
				return nil
			}
			if !d.Type().IsRegular() || copyVerified(f, dest) != nil {
				kept++
				return nil
			}
			_ = os.Remove(f)
			return nil
		})
		if kept == 0 {
			_ = os.RemoveAll(park) // only empty dirs are left, RemoveAll takes them
		}
	}
}
