package paths

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DayLayout is the UTC date shared by usage writers and readers.
const DayLayout = "2006-01-02"

// IsDayName accepts real calendar dates in DayLayout.
func IsDayName(name string) bool {
	_, err := time.Parse(DayLayout, name)
	return err == nil
}

// UsageOpenFrom is the first UTC day still open at instant now. Every reader
// and writer of the usage store computes its open/closed boundary from this
// one rule with the instant it acts on: a day named before the result is
// closed, and only a closed day may be compacted into its archive, while a day
// at or after it may still be written. One spelling means the compactor can
// never fold a day a reader or a writer still treats as open — a store whose
// clocks disagree would lose the day's rows to nobody's archive.
func UsageOpenFrom(now time.Time) string {
	return now.UTC().AddDate(0, 0, -1).Format(DayLayout)
}

// DayNames lists the sorted union of folders and archives in [first, last].
// An index without its JSONL still names a day: readers must detect incomplete publication.
func DayNames(base, first, last string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			switch {
			case strings.HasSuffix(name, ".jsonl.idx"):
				name = strings.TrimSuffix(name, ".jsonl.idx")
			case strings.HasSuffix(name, ".jsonl"):
				name = strings.TrimSuffix(name, ".jsonl")
			default:
				continue
			}
		}
		if name >= first && name <= last && IsDayName(name) {
			seen[name] = true
		}
	}
	days := make([]string, 0, len(seen))
	for name := range seen {
		days = append(days, name)
	}
	sort.Strings(days)
	return days
}

// UsageStoreLock blocks on usage/.move.lock; closing the file releases it.
// Readers share the lock, while relocation, compaction and closed-day writers exclude them.
func UsageStoreLock(base string, exclusive bool) (*os.File, error) {
	return usageStoreLock(base, exclusive, false)
}

// TryUsageStoreReadLock refuses a busy store instead of waiting to print a report.
func TryUsageStoreReadLock(base string) (*os.File, error) {
	return usageStoreLock(base, false, true)
}

// UsageLockWait bounds every hot path's wait for usage/.move.lock: a
// statusline, a Stop hook and an export answer or refuse; none queues behind a
// compaction's exclusive hold across minutes of I/O (INTENT 8).
const UsageLockWait = time.Second

// ErrUsageStoreBusy is the store lock still held when UsageLockWait ran out:
// a truthful refusal, never a hang.
var ErrUsageStoreBusy = fmt.Errorf("usage store busy: %w", syscall.EWOULDBLOCK)

// UsageStoreLockWithin takes the store lock, waiting at most wait for it.
// A store another process holds past the wait is refused with
// ErrUsageStoreBusy, which wraps EWOULDBLOCK, rather than blocking.
func UsageStoreLockWithin(base string, exclusive bool, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := usageStoreLock(base, exclusive, true)
		if err == nil || f != nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, ErrUsageStoreBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func usageStoreLock(base string, exclusive, nonblocking bool) (*os.File, error) {
	lockPath := filepath.Join(base, ".move.lock")
	var f *os.File
	var err error
	if !exclusive {
		f, err = os.Open(lockPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// A lock this process cannot open — a root-owned file left by one
			// sudo'd compaction — must not poison every reader forever. It is
			// never unlinked: a permission failure cannot show the holder is
			// stale, and replacing the inode under a live holder would split
			// the store's coordination in two. The reader answers without a
			// lock, as the create branch does; the archives are hash-bound and
			// every reader merges them with the folder.
			if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
				return nil, nil
			}
			return nil, err
		}
	}
	if f == nil {
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			if !exclusive && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)) {
				if _, statErr := os.Stat(lockPath); errors.Is(statErr, os.ErrNotExist) {
					return nil, nil // A read-only legacy store cannot publish a competing generation.
				}
			}
			return nil, err
		}
	}
	op := syscall.LOCK_SH
	if exclusive {
		op = syscall.LOCK_EX
	}
	if nonblocking {
		op |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), op)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// UsageArchiveRow restores a raw receipt's original filename and modification time.
type UsageArchiveRow struct {
	Name    string
	MTimeNS int64
	Data    []byte
}

// FileStamp is one file's identity: the inode the bytes belong to and the
// timestamps a rewrite moves. ctime is in the key because a rewrite can put
// size and mtime back and still be a different generation (#635).
type FileStamp struct {
	Dev   uint64
	Ino   uint64
	Size  int64
	MTime int64
	CTime int64
}

// ArchiveStamp is both files of one archive generation — the content and the
// index that binds it. A reader compares stamps without opening either file.
type ArchiveStamp struct {
	Data FileStamp
	Meta FileStamp
}

// UsageFileStamp is one file's identity from a stat the caller already took,
// with no byte read: the member a day's leftover folder joins to its mark.
func UsageFileStamp(info os.FileInfo) (FileStamp, bool) {
	if info == nil || !info.Mode().IsRegular() {
		return FileStamp{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileStamp{}, false
	}
	return FileStamp{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: info.Size(),
		MTime: info.ModTime().UnixNano(), CTime: archiveCtime(st)}, true
}

// String is the stamp's one spelling for the caches that file it.
func (s ArchiveStamp) String() string {
	return fmt.Sprintf("%x:%x:%d:%d:%d/%x:%x:%d:%d:%d",
		s.Data.Dev, s.Data.Ino, s.Data.Size, s.Data.MTime, s.Data.CTime,
		s.Meta.Dev, s.Meta.Ino, s.Meta.Size, s.Meta.MTime, s.Meta.CTime)
}

// UsageArchiveStamp is the archive pair's identity from one stat each, with no
// byte read: the check an unchanged pass pays instead of hashing every row
// (#559). ok is false when either file is missing or not a regular file — an
// orphan index or a half publication is not a generation to trust by stamp.
func UsageArchiveStamp(dayPath string) (ArchiveStamp, bool) {
	data, dataOK := archiveFileStamp(dayPath)
	meta, metaOK := archiveFileStamp(dayPath + ".idx")
	if !dataOK || !metaOK {
		return ArchiveStamp{}, false
	}
	return ArchiveStamp{Data: data, Meta: meta}, true
}

func archiveFileStamp(path string) (FileStamp, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return FileStamp{}, false
	}
	return UsageFileStamp(info)
}

// ArchiveSettle is how long a generation must have existed before an unchanged
// read is remembered: a pair just published can still be replaced by the
// writer that made it, so its ctime must be older than this before a memo
// trusts it. A variable so a test or a benchmark can make a fixture settled;
// production keeps the constant.
var ArchiveSettle = 2 * time.Second

// UsageArchiveLoaded, when set, is called with the day path on every read of an
// archive generation from disk — never on a memo hit. A test arms it to bound
// what a pass loads; it is nil in production.
var UsageArchiveLoaded func(dayPath string)

type archiveMemo struct {
	stamp ArchiveStamp
	rows  []UsageArchiveRow
}

var archiveMemos = struct {
	sync.Mutex
	byPath map[string]archiveMemo
}{byPath: map[string]archiveMemo{}}

// archiveMemoGet answers a remembered generation, and gives the caller its own
// slice: appending to a row's Data must not write into the memo's bytes, and a
// changed Name or MTime must not reach the next caller.
func archiveMemoGet(dayPath string, stamp ArchiveStamp) ([]UsageArchiveRow, bool) {
	archiveMemos.Lock()
	defer archiveMemos.Unlock()
	m, ok := archiveMemos.byPath[dayPath]
	if !ok || m.stamp != stamp {
		return nil, false
	}
	return callerRows(m.rows), true
}

// archiveMemoPut remembers a successful read once its generation has settled.
// A later read of the same identity is then a stat, not a byte.
func archiveMemoPut(dayPath string, stamp ArchiveStamp, rows []UsageArchiveRow) {
	if time.Since(time.Unix(0, max(stamp.Data.CTime, stamp.Meta.CTime))) < ArchiveSettle {
		return
	}
	archiveMemos.Lock()
	archiveMemos.byPath[dayPath] = archiveMemo{stamp: stamp, rows: callerRows(rows)}
	archiveMemos.Unlock()
}

// callerRows copies the row slice and cuts each Data with a 3-index slice, so
// a caller's append allocates instead of growing into the memo.
func callerRows(rows []UsageArchiveRow) []UsageArchiveRow {
	out := make([]UsageArchiveRow, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].Data = out[i].Data[:len(out[i].Data):len(out[i].Data)]
	}
	return out
}

// ReadUsageArchive reads usage/<day>.jsonl and its hash-bound .idx metadata.
// A successful generation is remembered by both files' identity, so an
// unchanged archive costs a stat rather than a read (#559); the memo is
// success-only and its identity includes ctime, so a rewrite is read again
// (#635). Callers hold UsageStoreLock across their folder/archive snapshot.
// Neither file present returns nil, nil; incomplete or mismatched generations
// return no rows and an error.
func ReadUsageArchive(dayPath string) ([]UsageArchiveRow, error) {
	stamp, haveStamp := UsageArchiveStamp(dayPath)
	if haveStamp {
		if rows, ok := archiveMemoGet(dayPath, stamp); ok {
			return rows, nil
		}
	}
	// Never waits on a FIFO left under an archive's name (#654).
	data, dataErr := ReadRegularFile(dayPath)
	meta, metaErr := ReadRegularFile(dayPath + ".idx")
	if UsageArchiveLoaded != nil && (dataErr == nil || metaErr == nil) {
		UsageArchiveLoaded(dayPath)
	}
	if errors.Is(dataErr, os.ErrNotExist) && errors.Is(metaErr, os.ErrNotExist) {
		return nil, nil
	}
	if errors.Is(dataErr, os.ErrNotExist) && metaErr == nil {
		// A first publication killed between its index and its content leaves an
		// index over no content: the day's rows are still whole in the folder (a
		// publication unlinks them only after both files land), so the index is
		// garbage, not a promise. Every first sight of it — reader or compactor,
		// under the store's locks — heals the day by clearing the garbage. A
		// folder that is not there is a different damage and stays an error.
		if info, err := os.Stat(strings.TrimSuffix(dayPath, ".jsonl")); err == nil && info.IsDir() {
			_ = os.Remove(dayPath + ".idx")
			return nil, nil
		}
	}
	if dataErr != nil {
		return nil, dataErr
	}
	if metaErr != nil {
		return nil, metaErr
	}
	type generation struct {
		SHA256 string `json:"sha256"`
		Files  []struct {
			Name    string `json:"name"`
			MTimeNS *int64 `json:"mtime_ns"`
		} `json:"files"`
	}
	var idx struct {
		V int `json:"v"`
		generation
		Previous *generation `json:"previous,omitempty"`
	}
	if err := json.Unmarshal(meta, &idx); err != nil {
		return nil, fmt.Errorf("%s: invalid archive index: %w", dayPath, err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if idx.SHA256 != digest && idx.Previous != nil && idx.Previous.SHA256 == digest {
		idx.generation = *idx.Previous // A crash between index and JSONL renames still has its old metadata.
	}
	if idx.V != 1 || idx.Files == nil || idx.SHA256 != digest {
		return nil, fmt.Errorf("%s: archive index version or hash mismatch", dayPath)
	}
	var lines [][]byte
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			return nil, fmt.Errorf("%s: incomplete archive line", dayPath)
		}
		lines = bytes.Split(data[:len(data)-1], []byte{'\n'})
	}
	if len(lines) != len(idx.Files) {
		return nil, fmt.Errorf("%s: archive row count mismatch", dayPath)
	}
	rows := make([]UsageArchiveRow, len(lines))
	for i, line := range lines {
		f := idx.Files[i]
		if f.Name == "" || filepath.Base(f.Name) != f.Name || strings.ContainsAny(f.Name, "/\\\x00") ||
			!strings.HasSuffix(f.Name, ".json") || f.MTimeNS == nil || len(bytes.TrimSpace(line)) == 0 ||
			(i > 0 && idx.Files[i-1].Name >= f.Name) {
			return nil, fmt.Errorf("%s: invalid archive row metadata at line %d", dayPath, i+1)
		}
		rows[i] = UsageArchiveRow{Name: f.Name, MTimeNS: *f.MTimeNS, Data: line}
	}
	// Remember the generation only if it did not move under this read: the
	// bytes just read belong to the stamp taken before them, and a pair that
	// changed in between is answered but never memoised.
	if haveStamp {
		if after, ok := UsageArchiveStamp(dayPath); ok && after == stamp {
			archiveMemoPut(dayPath, after, rows)
		}
	}
	return rows, nil
}
