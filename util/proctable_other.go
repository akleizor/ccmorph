//go:build !darwin

package util

// sysctlProcessTable is the darwin reader; elsewhere the ps path is all there is.
func sysctlProcessTable() (map[int]psRow, bool) { return nil, false }
