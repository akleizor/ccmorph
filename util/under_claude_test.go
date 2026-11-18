package util

import "testing"

func TestUnderClaude(t *testing.T) {
	tab := map[int]psRow{
		10: {ppid: 1, comm: "/Applications/Ghostty.app/Contents/MacOS/ghostty"},
		11: {ppid: 10, comm: "-/bin/zsh"},
		12: {ppid: 11, comm: "zsh", args: "zsh /opt/homebrew/bin/ccmorph run alex"},
		20: {ppid: 12, comm: "claude", args: "claude --effort medium"},
		21: {ppid: 20, comm: "/bin/zsh"},
		22: {ppid: 21, comm: "zsh", args: "zsh ccmorph run ds -p hi"},
		30: {ppid: 1, comm: "2.1.327", args: "/Users/a/.local/share/claude/versions/2.1.327 --print"},
		31: {ppid: 30, comm: "zsh"},
		40: {ppid: 1, comm: "/bin/sh", args: "/bin/sh /tmp/x/bin/claude"},
		41: {ppid: 40, comm: "zsh"},
		50: {ppid: 1, comm: "node", args: "node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"},
		51: {ppid: 50, comm: "zsh"},
	}
	for pid, want := range map[int]bool{12: false, 22: true, 31: true, 41: true, 51: true, 99: false} {
		if got := underClaude(tab, pid); got != want {
			t.Errorf("underClaude(%d) = %v, want %v", pid, got, want)
		}
	}
}
