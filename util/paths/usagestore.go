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

func usageStoreLock(base string, exclusive, nonblocking bool) (*os.File, error) {
	lockPath := filepath.Join(base, ".move.lock")
	var f *os.File
	var err error
	if !exclusive {
		f, err = os.Open(lockPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
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

// ReadUsageArchive reads usage/<day>.jsonl and its hash-bound .idx metadata.
// Callers hold UsageStoreLock across their folder/archive snapshot. Neither file
// present returns nil, nil; incomplete or mismatched generations return no rows and an error.
func ReadUsageArchive(dayPath string) ([]UsageArchiveRow, error) {
	data, dataErr := os.ReadFile(dayPath)
	meta, metaErr := os.ReadFile(dayPath + ".idx")
	if errors.Is(dataErr, os.ErrNotExist) && errors.Is(metaErr, os.ErrNotExist) {
		return nil, nil
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
	return rows, nil
}
