package shim

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akleizor/ccmorph/util/paths"
)

type storedUsageRow struct {
	paths.UsageArchiveRow
	file     fileID
	archived bool
}

// readUsageDay merges original names, retaining both rows on an identity collision.
// Its caller holds .move.lock while reading the folder and hash-bound archive.
func readUsageDay(dir string) ([]storedUsageRow, error) {
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
			return nil, err
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
				key += "\x00archive"
			}
		}
		rows[key] = storedUsageRow{UsageArchiveRow: row, file: fileID{archive: archivePath, line: i + 1}, archived: true}
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
		data, err := os.ReadFile(path)
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
