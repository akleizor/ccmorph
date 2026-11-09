package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpPage is one help page as usage prints it wrapped to width columns. The
// width is the caller's, so pages can be checked at 80 columns without a
// terminal; 0 is the width a pipe receives.
func helpPage(t *testing.T, topic string, width int) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := (&launcher{}).usageAt(topic, width)
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	if rc != 0 {
		t.Fatalf("help %q at %d columns: rc %d", topic, width, rc)
	}
	return string(b)
}

// GH-364: help pages were fixed lines with no regard for the terminal's
// width, so an 80-column terminal cut them inside words. Wrapped to 80, no
// line may be wider, and the words must be exactly the piped page's words:
// wrapping re-breaks lines, it never splits or drops a word.
func TestEveryHelpPageFitsEightyColumnsWithoutCuttingAWord(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no bridge on PATH: the plain add page
	topics := []string{"", "all"}
	for topic := range usageTopics {
		topics = append(topics, topic)
	}
	for _, topic := range topics {
		wide, piped := helpPage(t, topic, 80), helpPage(t, topic, 0)
		for _, line := range strings.Split(wide, "\n") {
			if cells(line) > 80 {
				t.Errorf("help %q at 80 columns: line of %d cells: %q", topic, cells(line), line)
			}
		}
		if got, want := strings.Join(strings.Fields(wide), " "), strings.Join(strings.Fields(piped), " "); got != want {
			t.Errorf("help %q wrapped to 80 lost or cut words:\n got: %q\nwant: %q", topic, got, want)
		}
	}
}

// GH-364: with a bridge installed, help add carries its longest line — the
// provider sentence. On an 80-column terminal it wraps as one list item, its
// continuation under the item's own indent, while a pipe still receives the
// single authored line.
func TestHelpAddWrapsTheProviderSentenceAsOneListItem(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	for _, p := range []string{"claude", "antigravity", "codex", "devin", "kimi-ai", "kimi", "meta", "xai"} {
		script += "echo '  -" + p + "-login'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "cliproxyapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	wide, piped := helpPage(t, "add", 80), helpPage(t, "add", 0)
	sentence := "  Antigravity, Codex, Devin, Kimi-ai, Kimi, Meta, xAI: sign in to that service with your browser (through cliproxyapi)."
	if !strings.Contains(piped, sentence) {
		t.Fatalf("piped help add lost the authored provider line:\n%s", piped)
	}
	lines := strings.Split(wide, "\n")
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  Antigravity,") {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("help add at 80 columns lost the provider sentence:\n%s", wide)
	}
	if want := "  Antigravity, Codex, Devin, Kimi-ai, Kimi, Meta, xAI: sign in to that service"; lines[first] != want {
		t.Errorf("provider line = %q, want %q", lines[first], want)
	}
	if want := "  with your browser (through cliproxyapi)."; lines[first+1] != want {
		t.Errorf("provider continuation = %q, want %q", lines[first+1], want)
	}
}

// GH-364: a description longer than the width left after its label column
// wraps under that column, the way the doctor row's description already sits
// under the column its long usage line opens.
func TestHelpAllWrapsALongDescriptionUnderItsOwnColumn(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lines := strings.Split(helpPage(t, "all", 80), "\n")
	row := -1
	for i, line := range lines {
		if strings.Contains(line, "Delete a profile") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("help all at 80 columns lost the rm row:\n%s", strings.Join(lines, "\n"))
	}
	col := strings.Index(lines[row], "Delete")
	if want := strings.Repeat(" ", col) + "state"; lines[row+1] != want {
		t.Errorf("rm row continues with %q, want %q", lines[row+1], want)
	}
}
