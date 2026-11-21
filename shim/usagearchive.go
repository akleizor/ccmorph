package shim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/util/paths"
)

type storedUsageRow struct {
	paths.UsageArchiveRow
	file fileID
}

// readUsageDay merges original names, retaining both rows on an identity collision.
// Its caller holds .move.lock while reading the folder and hash-bound archive.
func readUsageDay(dir string) ([]storedUsageRow, error) {
	return readUsageDayNoting(dir, nil)
}

// readUsageDayNoting is readUsageDay telling unreadable about each row that is
// still there but could not be read, so the report can name it the way quota
// and the export do (#654). A row gone between listing and read is the store's
// own relocation and is skipped without a word, as every reader skips it.
func readUsageDayNoting(dir string, unreadable func(path string, err error)) ([]storedUsageRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	rows := map[string]storedUsageRow{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, file, err := readRowFile(path)
		if err != nil {
			// One row this pass cannot read — gone between listing and open, or
			// owned by someone else — never stops the day: a hook pass is a
			// reading of what can be read. A row still there is told to the
			// caller, which decides whether a total can stand without it.
			if unreadable != nil && !errors.Is(err, os.ErrNotExist) {
				unreadable(path, err)
			}
			continue
		}
		rows[entry.Name()] = storedUsageRow{UsageArchiveRow: paths.UsageArchiveRow{Name: entry.Name(), MTimeNS: file.mtimeNS, Data: data}, file: file}
	}
	archivePath := dir + ".jsonl"
	archived, err := paths.ReadUsageArchive(archivePath)
	if err != nil {
		return nil, err
	}
	for i, row := range archived {
		key := row.Name
		if live, ok := rows[key]; ok {
			if sameNamedUsage(live.Data, row.Data) {
				if live.MTimeNS >= row.MTimeNS {
					continue
				}
			} else {
				key += paths.ArchiveCopySuffix
			}
		}
		rows[key] = storedUsageRow{UsageArchiveRow: row, file: fileID{archive: archivePath, line: i + 1}}
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]storedUsageRow, 0, len(keys))
	for _, key := range keys {
		out = append(out, rows[key])
	}
	return out, nil
}

// namesResponse is the archive scan's cheap filter: the row's bytes carry this
// response id. A #633 spill is found by its name; a spill a build before it
// wrote is found by the id the row carries, and sameResponse still decides
// whether the row is this response.
func namesResponse(data []byte, responseID string) bool {
	return bytes.Contains(data, []byte(responseID))
}

// usageRowsFor reads only the names one response can own, plus relay spills.
// Live days keep their one-file lookup; only a day with an archive pays its scan.
// legacy adds the spill names a build before #633 spelled under the joined
// key's hash, which only a reader holding the row's own turn can name: a pass
// that folds or retires its own earlier row asks for them, and the hook's
// held-primary check — the one lookup every response pays for — does not.
func usageRowsFor(dir, responseID, turnID string, allSpills, legacy bool) ([]storedUsageRow, error) {
	if paths.IsDayName(filepath.Base(dir)) {
		_, dataErr := os.Stat(dir + ".jsonl")
		_, metaErr := os.Stat(dir + ".jsonl.idx")
		if !errors.Is(dataErr, os.ErrNotExist) || !errors.Is(metaErr, os.ErrNotExist) {
			rows, err := readUsageDay(dir)
			if err != nil {
				return nil, err
			}
			primary, spills := rowNames(dir, responseID, turnID)
			if legacy {
				spills = append(spills, legacySpillNames(dir, responseID, turnID)...)
			}
			prefix := strings.TrimSuffix(filepath.Base(primary), ".json") + "."
			var out []storedUsageRow
			for _, row := range rows {
				match := row.Name == filepath.Base(primary)
				if allSpills {
					// A spill a build before #633 wrote is still under the
					// joined key's hash inside the archive: compaction copies
					// the name and migrateOldSpills cannot re-file an archived
					// row, and the relay's turn is not the reader's to name.
					// The day is read whole either way, so the row is found by
					// the response id it carries (sameResponse decides).
					match = match || strings.HasPrefix(row.Name, prefix) || namesResponse(row.Data, responseID)
				} else {
					for _, spill := range spills {
						match = match || row.Name == filepath.Base(spill)
					}
				}
				if match {
					out = append(out, row)
				}
			}
			return out, nil
		}
	}
	primary, spills := rowNames(dir, responseID, turnID)
	if allSpills {
		name := rowFileName(responseID)
		spills = spillsIn(dir, strings.TrimSuffix(name, ".json")+".", name)
	}
	if legacy {
		spills = append(spills, legacySpillNames(dir, responseID, turnID)...)
	}
	var rows []storedUsageRow
	for _, path := range append([]string{primary}, spills...) {
		data, err := paths.ReadRegularFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, storedUsageRow{UsageArchiveRow: paths.UsageArchiveRow{Name: filepath.Base(path), Data: data}})
	}
	return rows, nil
}

// ReadUsageWindow is every receipt the store holds in its flat top level and in
// the UTC days [first, last], read the way the report reads them: the flat
// receipts first, then each day's live folder merged with its archive by the
// store's one rule (readUsageDayNoting). The rows come back day folders first
// and flat last, the order quota's month read counts them in, so a response
// held under both names counts by its day folder's copy in every reader. A receipt still there that cannot be
// read is passed to unreadable and skipped; a name gone between its listing and
// its read is the store's own relocation, skipped without a word. The error is
// a surface the window cannot be read without: the store's listing, a day
// folder that cannot be listed and searched — unless its hash-verified archive
// answers the day alone, as quota's archivedDayRows lets it — or an archive
// that does not verify. quota answers from it when its cached read could not
// see every receipt, so a month with an unreadable receipt is the total the
// report and the export give (#654). The caller holds the store's shared lock.
func ReadUsageWindow(base, first, last string, unreadable func(path string, err error)) ([][]byte, error) {
	if err := searchable(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out, flat [][]byte
	for _, path := range flatRowFiles(base) {
		data, _, err := readRowFile(path)
		if err == nil {
			flat = append(flat, data)
		} else if !errors.Is(err, os.ErrNotExist) {
			unreadable(path, err)
		}
	}
	for _, name := range paths.DayNames(base, first, last) {
		dir := filepath.Join(base, name)
		if err := searchable(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			archived, aerr := paths.ReadUsageArchive(dir + ".jsonl")
			if aerr != nil || archived == nil {
				return nil, fmt.Errorf("%s: %w", dir, err)
			}
			for _, row := range archived {
				out = append(out, row.Data)
			}
			continue
		}
		rows, err := readUsageDayNoting(dir, unreadable)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, row.Data)
		}
	}
	return append(out, flat...), nil
}

// searchable is whether dir can be listed and the names it lists reached: a
// folder that lists but cannot be searched (mode 0400) names receipts no read
// can reach, which is the folder's damage, not one receipt's (#545).
func searchable(dir string) error {
	return syscall.Access(dir, 0x4|0x1) // R_OK|X_OK
}
