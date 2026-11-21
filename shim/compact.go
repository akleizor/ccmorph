package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// CompactBudget bounds a compaction a hot path takes (INTENT 8, #654). The zero
// value is unbounded: `ccmorph-core usage-compact` drains whatever is there.
type CompactBudget struct {
	// Deadline is checked before each day, before a day's publication and
	// before each removal; once it has passed, the walk stops and a later pass
	// finishes what this one left. Zero means no deadline.
	Deadline time.Time
	// MaxFiles leaves a day folder holding more entries than this whole, for
	// usage-compact; 0 means no cap.
	MaxFiles int
	// MaxBytes leaves a day whole, for usage-compact, when its existing
	// archive and index together, or its folder's receipts together, are
	// larger than this; 0 means no cap. Each receipt's stat is counted before
	// it is read and the read is held to what the cap has left, so a day over
	// the cap costs at most MaxBytes+1 bytes of reading, never its size.
	MaxBytes int64
}

// errOverBudget is a day this pass's budget does not cover: it is passed over
// whole, unchanged, for a later pass or usage-compact.
var errOverBudget = errors.New("usage day is over the compaction budget")

func (b CompactBudget) spent() bool {
	return !b.Deadline.IsZero() && !time.Now().Before(b.Deadline)
}

// overBudget is whether a day is too large for the budget. It reads at most
// MaxFiles+1 names and stats two files, so a hot path's gate costs the cap,
// never the day's size. A day it cannot list is not over: the day's own read
// names that error. The folder's bytes are readUsageFolder's to bound, as it
// reads.
func (b CompactBudget) overBudget(dir string) bool {
	if b.MaxFiles > 0 {
		if f, err := os.Open(dir); err == nil {
			names, _ := f.Readdirnames(b.MaxFiles + 1)
			f.Close()
			if len(names) > b.MaxFiles {
				return true
			}
		}
	}
	if b.MaxBytes > 0 {
		var size int64
		for _, path := range []string{dir + ".jsonl", dir + ".jsonl.idx"} {
			if info, err := os.Stat(path); err == nil {
				size += info.Size()
			}
		}
		if size > b.MaxBytes {
			return true
		}
	}
	return false
}

// CompactUsage compacts at most limit closed UTC day folders in base (the usage
// directory). Today and yesterday remain live; limit 0 drains eligible days.
// A busy .move.lock leaves compaction to its holder. No receipt is removed until
// the published archive has been reread and verified against the source bytes.
func CompactUsage(base string, now time.Time, limit int) (int, error) {
	return CompactUsageWithin(base, now, limit, CompactBudget{})
}

// CompactUsageWithin is CompactUsage held to budget: a day over the budget's
// size is passed over, and no step starts after its deadline.
func CompactUsageWithin(base string, now time.Time, limit int, budget CompactBudget) (int, error) {
	if limit < 0 {
		return 0, errors.New("usage compaction limit must be nonnegative")
	}
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	lock, ok := moveLock(base)
	if !ok {
		return 0, nil
	}
	defer lock.Close()
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, err
	}
	before := paths.UsageOpenFrom(now)
	count := 0
	var failed []error
	for _, entry := range entries {
		if !entry.IsDir() || !paths.IsDayName(entry.Name()) || entry.Name() >= before {
			continue
		}
		if budget.spent() {
			break
		}
		changed, err := compactDay(filepath.Join(base, entry.Name()), budget)
		if err != nil {
			// One day this pass cannot rebuild is named and passed over: the
			// oldest-first walk may never let a damaged day wedge every newer
			// day behind it forever. The failures join into the walk's error.
			failed = append(failed, err)
			continue
		}
		if changed {
			count++
			if limit > 0 && count >= limit {
				break
			}
		}
	}
	return count, errors.Join(failed...)
}

// UsageCompactMain is ccmorph-core usage-compact, with no arguments.
func UsageCompactMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: ccmorph-core usage-compact")
		return 2
	}
	n, err := CompactUsage(usageBase(), time.Now(), 0)
	fmt.Fprintf(stdout, "compacted %d usage day(s)\n", n)
	if err != nil {
		fmt.Fprintln(stderr, "usage-compact:", err)
		return 1
	}
	return 0
}

// compactPublished is the boundary before the independent reread; tests corrupt
// the actual published bytes here, never the verification result.
var compactPublished = func(string) {}

func compactDay(dir string, budget CompactBudget) (changed bool, err error) {
	if budget.overBudget(dir) {
		return false, nil
	}
	archivePath := dir + ".jsonl"
	folder, err := readUsageFolder(dir, budget)
	if errors.Is(err, errUsageHardlinks) {
		return false, nil // Preserve inode identity until the interrupted move is resolved.
	}
	if errors.Is(err, errOverBudget) {
		return false, nil // Nothing is written yet: usage-compact or a later pass takes the day.
	}
	if err != nil {
		return false, err
	}
	archived, err := paths.ReadUsageArchive(archivePath)
	if err != nil {
		if recoverErr := recoverFirstArchive(archivePath, folder); recoverErr != nil {
			return false, errors.Join(err, recoverErr)
		}
		archived, err = paths.ReadUsageArchive(archivePath)
		if err != nil {
			return false, err
		}
	}
	if len(folder) == 0 {
		return false, nil
	}
	rows, dropped, multiline := mergeDayRows(dir, archived, folder)
	if multiline {
		// A receipt split over lines is still a row every reader counts from
		// the folder; the archive's one-line format cannot hold its bytes.
		fmt.Fprintf(os.Stderr, "usage-compact: %s holds a multiline receipt; it stays in the folder\n", dir)
	}
	if budget.spent() {
		return false, nil // Nothing is written yet: a later pass starts the day over.
	}
	publish := !equalGenerations(archived, rows)
	if publish {
		// Roll back a failed publication/verification while every folder row remains.
		verifiedPublication := false
		defer func() {
			if err == nil || verifiedPublication || !publish {
				return
			}
			if archived != nil {
				active, _ := paths.ReadUsageArchive(archivePath)
				err = errors.Join(err, writeUsageArchiveWithPrevious(archivePath, archived, active))
			} else {
				for _, path := range []string{archivePath, archivePath + ".idx"} {
					if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
						err = errors.Join(err, e)
					}
				}
				err = errors.Join(err, syncUsageDir(filepath.Dir(dir)))
			}
		}()
		if err = writeUsageArchiveWithPrevious(archivePath, rows, archived); err != nil {
			return false, err
		}
		compactPublished(archivePath)
		verified, verr := paths.ReadUsageArchive(archivePath)
		if verr != nil {
			return false, verr
		}
		if !equalGenerations(rows, verified) {
			return false, fmt.Errorf("%s: archive byte or identity verification failed", dir)
		}
		verifiedPublication = true
	}
	// A folder that moved while this pass published belongs to a later pass.
	current, err := readUsageFolder(dir, budget)
	if errors.Is(err, errOverBudget) {
		// The verified archive and the whole folder both stand, which readers
		// merge; the next pass removes the rows.
		return publish, nil
	}
	if err != nil {
		return publish, err
	}
	if !equalGenerations(folder, current) {
		return publish, fmt.Errorf("%s: folder changed during compaction", dir)
	}
	// Publication is durable and verified. Remove only rows the archive now
	// holds under their name: a row the archive refused stays where readers
	// still see it. A failed unlink leaves the verified archive plus its source
	// copy; readers merge the original filename once, and a later pass retries.
	holding := make(map[string][]byte, len(rows))
	for _, row := range rows {
		holding[row.Name] = row.Data
	}
	unlinked := 0
	for _, row := range folder {
		if budget.spent() {
			// Every row left is still in the verified archive or the folder,
			// which readers merge; the next pass removes the rest.
			return publish || unlinked > 0, syncUsageDir(dir)
		}
		data, ok := holding[row.Name]
		if !ok || (!bytes.Equal(data, row.Data) && !sameNamedUsage(data, row.Data)) {
			continue
		}
		if err := remove(filepath.Join(dir, row.Name)); err != nil {
			return true, err
		}
		unlinked++
	}
	// A row no reader counts — zero bytes from a crash inside the writer's
	// claim, a truncated write, anything that is not one JSON object — leaves
	// the folder so the day can close, and its bytes are the record of it.
	for _, row := range dropped {
		if budget.spent() {
			break
		}
		kept, err := dropRow(dir, row)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "usage-compact: %s: a row no reader counts stays in the folder: %v\n", filepath.Join(dir, row.Name), err)
			continue
		}
		fmt.Fprintf(os.Stderr, "usage-compact: dropped %s, a row no reader counts (%d bytes); kept at %s\n", filepath.Join(dir, row.Name), len(row.Data), kept)
		unlinked++
	}
	if err := syncUsageDir(dir); err != nil {
		return true, err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return true, err
	}
	return publish || unlinked > 0, syncUsageDir(filepath.Dir(dir))
}

// mergeDayRows folds a day's folder rows into its archived rows exactly as
// every reader reads the day: one row per original name when the identities
// agree (the newer stamp wins), and both rows when they conflict — the folder
// copy keeps the original name and the archived copy keeps a distinct
// spill-style name, the same keep-both the readers spell with their archive
// suffix. A folder row no reader counts (not one JSON object) comes back in
// dropped; a multiline receipt, which readers count but one archive line
// cannot hold, stays in the folder and sets multiline. The rows are
// name-sorted for the hash-bound publication.
func mergeDayRows(dir string, archived, folder []paths.UsageArchiveRow) (rows, dropped []paths.UsageArchiveRow, multiline bool) {
	merged := make(map[string]paths.UsageArchiveRow, len(archived)+len(folder))
	for _, row := range archived {
		merged[row.Name] = row
	}
	for _, row := range folder {
		normalized := nonFiniteToNull(row.Data)
		if !json.Valid(normalized) || parseDoc(normalized) == nil {
			dropped = append(dropped, row)
			continue
		}
		if bytes.ContainsRune(row.Data, '\n') {
			multiline = true
			continue
		}
		if old, ok := merged[row.Name]; ok {
			if !sameNamedUsage(old.Data, row.Data) {
				// The archive name is internal; conflicting identities remain two
				// turns, as in every reader's keep-both. The archived copy keeps
				// its bytes and stamp under a derived spill-style name.
				stem := strings.TrimSuffix(row.Name, ".json")
				kept := old
				for k := 1; ; k++ {
					kept.Name = fmt.Sprintf("%s.%d.json", stem, k)
					if _, taken := merged[kept.Name]; !taken {
						merged[kept.Name] = kept
						break
					}
				}
			} else if old.MTimeNS > row.MTimeNS {
				continue
			}
		}
		merged[row.Name] = row
	}
	rows = make([]paths.UsageArchiveRow, 0, len(merged))
	for _, row := range merged {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, dropped, multiline
}

// dropRow moves a row no reader counts from its day folder to
// <state>/ccmorph/usage-dropped/<day>/<mtime_ns>-<name>, outside every
// reader's listing, and returns where it went. It never replaces a kept row.
func dropRow(dir string, row paths.UsageArchiveRow) (string, error) {
	keep := filepath.Join(filepath.Dir(filepath.Dir(dir)), "usage-dropped", filepath.Base(dir))
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return "", err
	}
	kept := filepath.Join(keep, fmt.Sprintf("%d-%s", row.MTimeNS, row.Name))
	if _, err := os.Lstat(kept); err == nil {
		return "", fmt.Errorf("%s is already kept", kept)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(filepath.Join(dir, row.Name), kept); err != nil {
		return "", err
	}
	_ = syncUsageDir(keep) // The record is best effort; the folder's own sync follows.
	return kept, nil
}

// equalGenerations is whether a publication would write what the archive already
// holds: the day's cache marks move only on name, stamp or byte changes.
func equalGenerations(a, b []paths.UsageArchiveRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].MTimeNS != b[i].MTimeNS || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

func writeUsageArchiveWithPrevious(path string, rows, previous []paths.UsageArchiveRow) error {
	var data []byte
	type file struct {
		Name    string `json:"name"`
		MTimeNS int64  `json:"mtime_ns"`
	}
	files := make([]file, len(rows))
	for i, row := range rows {
		data = append(data, row.Data...)
		data = append(data, '\n')
		files[i] = file{row.Name, row.MTimeNS}
	}
	sum := sha256.Sum256(data)
	type generation struct {
		SHA256 string `json:"sha256"`
		Files  []file `json:"files"`
	}
	var prior *generation
	if previous != nil {
		var oldData []byte
		oldFiles := make([]file, len(previous))
		for i, row := range previous {
			oldData = append(oldData, row.Data...)
			oldData = append(oldData, '\n')
			oldFiles[i] = file{row.Name, row.MTimeNS}
		}
		oldSum := sha256.Sum256(oldData)
		prior = &generation{hex.EncodeToString(oldSum[:]), oldFiles}
	}
	meta, err := json.Marshal(struct {
		V        int         `json:"v"`
		SHA256   string      `json:"sha256"`
		Files    []file      `json:"files"`
		Previous *generation `json:"previous,omitempty"`
	}{1, hex.EncodeToString(sum[:]), files, prior})
	if err != nil {
		return err
	}
	for _, out := range []struct {
		path string
		data []byte
	}{{path + ".idx", meta}, {path, data}} {
		if err := paths.AtomicWrite(paths.WriteIn{Path: out.path, Temp: ".compact-*.tmp", Data: out.data, Mode: 0o600, Sync: true}); err != nil {
			return err
		}
		if err := syncUsageDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func syncUsageDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

var errUsageHardlinks = errors.New("receipt has multiple hardlinks")

// readUsageFolder reads a day folder's receipts. Under a budget it stops with
// errOverBudget before any read once the deadline has passed, and once the
// receipts' bytes would pass MaxBytes: each receipt's stat is counted before
// it is read and the read is held to one byte past what the cap has left, so
// it never reads more than MaxBytes+1 bytes whatever the files claim or
// become. The zero budget reads every receipt whole, as usage-compact must.
func readUsageFolder(dir string, budget CompactBudget) ([]paths.UsageArchiveRow, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []paths.UsageArchiveRow
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if budget.spent() {
			return nil, errOverBudget
		}
		// OpenRegular never waits on the open: a FIFO under a receipt's name
		// would otherwise hold this pass, and the store's exclusive lock, until
		// a writer appeared (INTENT 8, #654). Not a regular file fails the day,
		// which the walk names and passes over.
		f, info, err := paths.OpenRegular(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: invalid receipt file %s: %w", dir, entry.Name(), err)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink > 1 {
			f.Close()
			return nil, errUsageHardlinks
		}
		var r io.Reader = f
		if budget.MaxBytes > 0 {
			left := budget.MaxBytes - total
			if info.Size() > left {
				f.Close()
				return nil, errOverBudget
			}
			r = io.LimitReader(f, left+1)
		}
		data, err := io.ReadAll(r)
		cerr := f.Close()
		if err != nil || cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		total += int64(len(data))
		if budget.MaxBytes > 0 && total > budget.MaxBytes {
			return nil, errOverBudget
		}
		rows = append(rows, paths.UsageArchiveRow{Name: entry.Name(), MTimeNS: info.ModTime().UnixNano(), Data: data})
	}
	return rows, nil
}

// A killed first publication can rebuild only from a complete, unchanged source
// whose reconstructed bytes and metadata match the orphan index exactly.
func recoverFirstArchive(path string, folder []paths.UsageArchiveRow) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("archive is not an orphan index")
	}
	meta, err := os.ReadFile(path + ".idx")
	if err != nil {
		return err
	}
	var idx struct {
		V      int    `json:"v"`
		SHA256 string `json:"sha256"`
		Files  []struct {
			Name    string `json:"name"`
			MTimeNS int64  `json:"mtime_ns"`
		} `json:"files"`
	}
	if err := json.Unmarshal(meta, &idx); err != nil {
		return err
	}
	var data []byte
	if idx.V != 1 || len(folder) == 0 || len(idx.Files) != len(folder) {
		return errors.New("orphan index has no complete source")
	}
	for i, row := range folder {
		if bytes.ContainsRune(row.Data, '\n') || row.Name != idx.Files[i].Name || row.MTimeNS != idx.Files[i].MTimeNS {
			return errors.New("orphan index source changed")
		}
		data = append(data, row.Data...)
		data = append(data, '\n')
	}
	sum := sha256.Sum256(data)
	if idx.SHA256 != hex.EncodeToString(sum[:]) {
		return errors.New("orphan index source hash mismatch")
	}
	if err := paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".compact-*.tmp", Data: data, Mode: 0o600, Sync: true}); err != nil {
		return err
	}
	return syncUsageDir(filepath.Dir(path))
}

// sameNamedUsage is util/paths.SameNamedRows — the one merge rule every
// reader of the store shares, so a compaction can never merge rows a reader
// would have kept apart, or keep apart rows a reader would merge (#654).
func sameNamedUsage(a, b []byte) bool {
	return paths.SameNamedRows(a, b)
}
