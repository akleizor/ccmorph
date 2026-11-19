package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestUsageRowStopHelper is the child half of TestStopCheckpointsThePass: the
// real UsageRowMain with its second row blocked, so the parent can stop the
// pass with a real signal the way Claude Code's hook timeout does.
func TestUsageRowStopHelper(t *testing.T) {
	if os.Getenv("CCMORPH_USAGE_ROW_HELPER") != "1" {
		return
	}
	writeRow = func(p, pr, id, m, ts, s, turn string, u usageRow) (bool, error) {
		if id == "msg_2" { // the row in flight when the pass is stopped
			os.Stdout.WriteString("blocked\n")
			select {} // the stop handler's os.Exit ends the process
		}
		return WriteUsageRow(p, pr, id, m, ts, s, turn, u)
	}
	in, _ := json.Marshal(map[string]string{
		"transcript_path": os.Getenv("CCMORPH_USAGE_ROW_TRANSCRIPT"),
		"hook_event_name": "Stop",
	})
	os.Exit(UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, bytes.NewReader(in)))
}

// #645: the checkpoint a real SIGTERM writes, not one emulated inline. The pass
// writes msg_1's row, blocks on msg_2, and is stopped. The handler must leave
// the cursor after the row on disk and at the in-flight row's start, so the
// next pass neither rereads msg_1 nor skips msg_2.
func TestStopCheckpointsThePass(t *testing.T) {
	usageDir := stateHome(t)
	line1 := transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00Z", `{"input_tokens":1,"output_tokens":2}`)
	path := writeTranscript(t, line1,
		transcriptLine("u2", "msg_2", "claude-opus-5-5", "2026-11-05T10:00:01Z", `{"input_tokens":1,"output_tokens":2}`),
	)

	cmd := exec.Command(os.Args[0], "-test.run=TestUsageRowStopHelper")
	cmd.Env = append(os.Environ(),
		"CCMORPH_USAGE_ROW_HELPER=1",
		"CCMORPH_USAGE_ROW_TRANSCRIPT="+path,
	)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	blocked, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || blocked != "blocked\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper never reached the row in flight: %q, %v; stderr:\n%s", blocked, err, stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_ = cmd.Wait() // the handler exits 1; the cursor it left is what this reads

	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("stopped helper exited %d, want 1; stderr:\n%s", code, stderr.String())
	}
	if got := len(rows(t, usageDir)); got != 1 {
		t.Fatalf("rows on disk %d, want 1 before the stop", got)
	}
	if c := readCursor(cursorFile(path)); c.Offset != int64(len(line1)) {
		t.Fatalf("checkpoint %d, want %d (past the written row, at the row in flight)", c.Offset, len(line1))
	}
}

// TestCursorLockHelper is the child half of
// TestCheckpointCursorHoldsTheCursorLockAcrossProcesses: it pauses inside
// checkpointCursor's read/write window, holding whatever lock that update takes.
func TestCursorLockHelper(t *testing.T) {
	if os.Getenv("CCMORPH_CURSOR_LOCK_HELPER") != "1" {
		return
	}
	cursorHeld = func() {
		os.Stdout.WriteString("read\n")
		_, _ = io.ReadAll(os.Stdin)
	}
	checkpointCursor(&checkpoint{path: os.Getenv("CCMORPH_CURSOR_LOCK_PATH"), c: cursor{Offset: 25}})
	os.Exit(0)
}

// #645: the read/check/write that keeps a stop's checkpoint from regressing is
// one update across processes, not just across goroutines. The helper is paused
// between its read and its write; a newer pass's write must not be able to enter
// there, so the lock the update takes is held and a non-blocking take fails.
func TestCheckpointCursorHoldsTheCursorLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	writeCursor(path, cursor{Offset: 0})

	cmd := exec.Command(os.Args[0], "-test.run=TestCursorLockHelper")
	cmd.Env = append(os.Environ(),
		"CCMORPH_CURSOR_LOCK_HELPER=1",
		"CCMORPH_CURSOR_LOCK_PATH="+path,
	)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	read, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || read != "read\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper never reached the checkpoint's write: %q, %v", read, err)
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	took := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	lock.Close()
	if took == nil {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("checkpoint cursor did not hold its lock between reading and writing")
	}

	_ = in.Close() // the helper writes its checkpoint and exits
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if c := readCursor(path); c.Offset != 25 {
		t.Fatalf("cursor %d after the checkpoint, want 25", c.Offset)
	}
}
