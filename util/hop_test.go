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
