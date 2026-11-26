//go:build !linux && !darwin

package adapter

import (
	"errors"
	"os/exec"
)

// sigRecordSupported: neither a pending set nor a kernel signal event is
// exposed here, so the stubborn child's TERM cannot be read.
const sigRecordSupported = false

// sigBlockSupported: there is no record to check a block with.
const sigBlockSupported = false

// startStubborn is start: the child's ignored TERM keeps it stubborn.
func startStubborn(cmd *exec.Cmd) (*proc, error) { return start(cmd) }

type sigWatch struct{}

func watchChildSignals(int) (*sigWatch, error) { return &sigWatch{}, nil }

func (w *sigWatch) pending() (uint64, error) {
	return 0, errors.New("no signal record on this OS")
}

func (w *sigWatch) blocked() (uint64, error) {
	return 0, errors.New("no signal record on this OS")
}

func (w *sigWatch) close() {}
