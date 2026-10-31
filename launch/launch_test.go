package launch

import (
	"slices"
	"testing"
)

func TestSplitNULKeepsEmptyValues(t *testing.T) {
	if got := splitNUL("a\x00\x00b c\x00."); !slices.Equal(got, []string{"a", "", "b c"}) {
		t.Fatalf("got %q", got)
	}
	if got := splitNUL("."); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestEnvHelpersReadTheLastValue(t *testing.T) {
	env := []string{"A=1", "B=x=y", "A=2"}
	if v, ok := envGet(env, "A"); !ok || v != "2" {
		t.Fatalf("A=%q %v", v, ok)
	}
	if v, _ := envGet(env, "B"); v != "x=y" {
		t.Fatalf("B=%q", v)
	}
	if got := envDrop(env, "A"); !slices.Equal(got, []string{"B=x=y"}) {
		t.Fatalf("drop: %q", got)
	}
	if authKey([]string{"ANTHROPIC_AUTH_TOKEN=t"}) != "t" || authKey([]string{"ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k"}) != "k" {
		t.Fatal("authKey must prefer the API key")
	}
}

func TestProjectionStripsEveryLaunchKey(t *testing.T) {
	got := projectionArgs([]string{"--acw", "none"}, []string{"A=1", "junk", "B="})
	if !slices.Equal(got, []string{"--acw", "none", "--strip", "A", "--strip", "B"}) {
		t.Fatalf("got %q", got)
	}
}

func TestNestedEdgeEnvOnlyBehindTheLoopbackEdge(t *testing.T) {
	l := &launcher{}
	t.Setenv("CCMORPH_EDGE_ROUTE_ENV", "X=1\nY=2\nZ=3")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTPS_PROXY", "http://example.com:8080")
	if got := l.nestedEdgeEnv("X"); got != nil {
		t.Fatalf("a remote proxy must add nothing, got %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47670/")
	if got := l.nestedEdgeEnv("X", "Z"); !slices.Equal(got, []string{"X=1", "Z=3"}) {
		t.Fatalf("got %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:70000")
	if got := l.nestedEdgeEnv("X"); got != nil {
		t.Fatalf("an out-of-range port must add nothing, got %q", got)
	}
}
