package util

import "testing"

func TestHopTargetIsTheFirstNonShellBelowTheRelay(t *testing.T) {
	// relay 100 -> zsh 101 (forwarder) -> claude 102 -> zsh 103 (command shell) -> zsh 104 ($$)
	tab := map[int]psRow{
		101: {ppid: 100, comm: "/bin/zsh"},
		102: {ppid: 101, comm: "/opt/bin/claude"},
		103: {ppid: 102, comm: "-zsh"},
		104: {ppid: 103, comm: "zsh"},
	}
	if got := findHopTarget(tab, 100, 104); got != 102 {
		t.Fatalf("got %d, want 102", got)
	}
	if got := findHopTarget(tab, 999, 104); got != 0 {
		t.Fatalf("an unrelated relay must find nothing, got %d", got)
	}
	tab[101] = psRow{ppid: 100, comm: "node"}
	if got := findHopTarget(tab, 100, 104); got != 101 {
		t.Fatalf("nearest the relay wins, got %d", got)
	}
}

func TestHopTargetSkipsTheCoreLauncher(t *testing.T) {
	// The Go launcher runs claude as its child, so the process between the relay
	// and Claude Code is the core itself: relay 200 -> core 201 -> claude 202 ->
	// zsh 203 (command shell) -> zsh 204 ($$). Ending the core forwards TERM to
	// claude as a signal the launch sent, which discards the queued switch.
	tab := map[int]psRow{
		201: {ppid: 200, comm: "/Users/u/.config/ccmorph/libexec/ccmorph-core"},
		202: {ppid: 201, comm: "/opt/bin/claude"},
		203: {ppid: 202, comm: "zsh"},
		204: {ppid: 203, comm: "-zsh"},
	}
	if got := findHopTarget(tab, 200, 204); got != 202 {
		t.Fatalf("the hop must end claude (202), not the core launcher, got %d", got)
	}
	tab[202] = psRow{ppid: 201, comm: "claude"}
	if got := findHopTarget(tab, 200, 204); got != 201+1 {
		t.Fatalf("a bare comm name must be found too, got %d", got)
	}
	// A core with nothing but shells below it has no Claude Code to end.
	tab[202] = psRow{ppid: 201, comm: "sh"}
	if got := findHopTarget(tab, 200, 204); got != 0 {
		t.Fatalf("shells below the core are not a hop target, got %d", got)
	}
}
