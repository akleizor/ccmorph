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

// CompactUsage compacts at most limit closed UTC day folders in base (the usage
// directory). Today and yesterday remain live; limit 0 drains eligible days.
// A busy .move.lock leaves compaction to its holder. No receipt is removed until
// the published archive has been reread and verified against the source bytes.
func CompactUsage(base string, now time.Time, limit int) (int, error) {
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
	before := now.UTC().AddDate(0, 0, -1).Format(paths.DayLayout)
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !paths.IsDayName(entry.Name()) || entry.Name() >= before {
			continue
		}
		changed, err := compactDay(filepath.Join(base, entry.Name()))
		if err != nil {
			return count, err
		}
		if changed {
			count++
			if limit > 0 && count >= limit {
				break
			}
		}
	}
	return count, nil
}

// UsageCompactMain is ccmorph-core usage-compact, with no arguments.
func UsageCompactMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: ccmorph-core usage-compact")
		return 2
	}
	n, err := CompactUsage(usageBase(), time.Now(), 0)
	if err != nil {
		fmt.Fprintln(stderr, "usage-compact:", err)
		return 1
	}
	fmt.Fprintf(stdout, "compacted %d usage day(s)\n", n)
	return 0
}

// compactPublished is the boundary before the independent reread; tests corrupt
// the actual published bytes here, never the verification result.
var compactPublished = func(string) {}

func compactDay(dir string) (changed bool, err error) {
	archivePath := dir + ".jsonl"
	folder, err := readUsageFolder(dir)
	if errors.Is(err, errUsageHardlinks) {
		return false, nil // Preserve inode identity until the interrupted move is resolved.
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
	merged := make(map[string]paths.UsageArchiveRow, len(archived)+len(folder))
	for _, row := range archived {
		merged[row.Name] = row
	}
	for _, row := range folder {
		if old, ok := merged[row.Name]; ok {
			if !sameNamedUsage(old.Data, row.Data) {
				return false, fmt.Errorf("%s: conflicting receipt identities for %s", dir, row.Name)
			}
			if old.MTimeNS > row.MTimeNS {
				continue
			}
		}
		merged[row.Name] = row
	}
	rows := make([]paths.UsageArchiveRow, 0, len(merged))
	for _, row := range merged {
		if bytes.ContainsRune(row.Data, '\n') || !json.Valid(nonFiniteToNull(row.Data)) || parseDoc(nonFiniteToNull(row.Data)) == nil {
			return false, fmt.Errorf("%s: %s is not a single raw JSON object line", dir, row.Name)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	// Roll back a failed publication/verification while every folder row remains.
	verifiedPublication := false
	defer func() {
		if err == nil || verifiedPublication {
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
	verified, err := paths.ReadUsageArchive(archivePath)
	if err != nil {
		return false, err
	}
	if !equalUsageRows(rows, verified) {
		return false, fmt.Errorf("%s: archive byte or identity verification failed", dir)
	}
	current, err := readUsageFolder(dir)
	if err != nil {
		return false, err
	}
	if !equalUsageRows(folder, current) {
		return false, fmt.Errorf("%s: folder changed during compaction", dir)
	}
	verifiedPublication = true
	// Publication is durable and verified. A failed unlink leaves the verified
	// archive plus its source copy; readers merge the original filename once.
	for _, row := range folder {
		if err := remove(filepath.Join(dir, row.Name)); err != nil {
			return true, err
		}
	}
	if err := syncUsageDir(dir); err != nil {
		return true, err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return true, err
	}
	return true, syncUsageDir(filepath.Dir(dir))
}

func writeUsageArchive(path string, rows []paths.UsageArchiveRow) error {
	return writeUsageArchiveWithPrevious(path, rows, nil)
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

func readUsageFolder(dir string) ([]paths.UsageArchiveRow, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []paths.UsageArchiveRow
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, fmt.Errorf("%s: invalid receipt file %s: %v", dir, entry.Name(), err)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink > 1 {
			f.Close()
			return nil, errUsageHardlinks
		}
		data, err := io.ReadAll(f)
		cerr := f.Close()
		if err != nil || cerr != nil {
			return nil, errors.Join(err, cerr)
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

func equalUsageRows(a, b []paths.UsageArchiveRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].MTimeNS != b[i].MTimeNS || !bytes.Equal(a[i].Data, b[i].Data) || !sameNamedUsage(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// A missing legacy turn is not evidence that a recycled response id is one row.
func sameNamedUsage(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	da, db := parseDoc(nonFiniteToNull(a)), parseDoc(nonFiniteToNull(b))
	if da == nil || db == nil {
		return false
	}
	ia, oka := da.str("response_id")
	ib, okb := db.str("response_id")
	if !oka || !okb || ia == "" || ia != ib {
		return false
	}
	ta, taOK := da.str("turn_id")
	tb, tbOK := db.str("turn_id")
	_, taPresent := da.last("turn_id")
	_, tbPresent := db.last("turn_id")
	if (taPresent && !taOK) || (tbPresent && !tbOK) || ta != tb {
		return false
	}
	if ta == "" {
		sa, oka := da.str("timestamp")
		sb, okb := db.str("timestamp")
		return oka && okb && sa == sb
	}
	return true
}
