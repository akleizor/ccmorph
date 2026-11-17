package shim

import (
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

// usageRowsFor reads only the names one response can own, plus relay spills.
// Live days keep their one-file lookup; only a day with an archive pays its scan.
func usageRowsFor(dir, responseID, turnID string, allSpills bool) ([]storedUsageRow, error) {
	if paths.IsDayName(filepath.Base(dir)) {
		_, dataErr := os.Stat(dir + ".jsonl")
		_, metaErr := os.Stat(dir + ".jsonl.idx")
		if !errors.Is(dataErr, os.ErrNotExist) || !errors.Is(metaErr, os.ErrNotExist) {
			rows, err := readUsageDay(dir)
			if err != nil {
				return nil, err
			}
			primary, spills := rowNames(dir, responseID, turnID)
			prefix := strings.TrimSuffix(filepath.Base(primary), ".json") + "."
			var out []storedUsageRow
			for _, row := range rows {
				match := row.Name == filepath.Base(primary)
				if allSpills {
					match = match || strings.HasPrefix(row.Name, prefix)
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
		spills = spillsIn(dir, strings.TrimSuffix(rowFileName(responseID), ".json")+".")
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
