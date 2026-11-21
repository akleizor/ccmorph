package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPriceRejectsUnknownOptions(t *testing.T) {
	for _, option := range []string{"--jsoon", "-j"} {
		t.Run(option, func(t *testing.T) {
			stderr, rc := capturePriceStderr(t, func() int {
				return (&launcher{}).verb([]string{"price", option, "claude-sonnet-4-5"})
			})
			if rc == 0 || !strings.Contains(stderr, option) || !strings.Contains(stderr, "usage:") || !strings.Contains(stderr, "--") {
				t.Fatalf("rc %d, stderr %q; want non-zero usage error naming %q and explaining --", rc, stderr, option)
			}
		})
	}
}

func TestPriceDoubleDashPassesDashPrefixedModel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := filepath.Join(root, "ccmorph")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"host":"owned:any","fetched":0,"models":{"-claude-model":{"input":0.15,"output":0.6}}}`
	if err := os.WriteFile(filepath.Join(dir, "prices-owned-any.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := captureStdout(t, func() int {
		return (&launcher{}).verb([]string{"price", "--", "-claude-model"})
	})
	if rc != 0 || out != "-claude-model  $0.15/$0.6 per 1M tokens\n" {
		t.Fatalf("rc %d, output %q", rc, out)
	}
}

func TestPriceHelpFlagStopsAtTheDelimiter(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"before", []string{"price", "--help"}, "Usage: ccmorph price "},
		{"after", []string{"price", "--", "--help"}, "--help  no cached price\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CCMORPH_STATE_DIR", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			out, code := captureStdout(t, func() int { return fromEnv().dispatch(c.args) })
			if code != 0 || !strings.Contains(out, c.want) {
				t.Fatalf("args %v: exit %d, stdout %q; want it to contain %q", c.args, code, out, c.want)
			}
		})
	}
}

func capturePriceStderr(t *testing.T, f func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	rc := f()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), rc
}

func TestPriceJSON(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := filepath.Join(root, "ccmorph")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"host":"owned:any","fetched":0,"models":{"model":{"input":0.15,"output":0.6}}}`
	if err := os.WriteFile(filepath.Join(dir, "prices-owned-any.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := captureStdout(t, func() int {
		return (&launcher{}).verb([]string{"price", "--json", "model-FP8", "missing"})
	})
	if rc != 0 || out != "{\"model-FP8\":{\"input\":0.15,\"output\":0.6}}\n" {
		t.Fatalf("rc %d, output %q", rc, out)
	}
}

func TestPriceEmptyCache(t *testing.T) {
	const hint = "Run a `provider` profile with `ccmorph run PROFILE`; its launch refreshes this cache in the background."
	for _, tc := range []struct {
		name  string
		cache string
		args  []string
		json  bool
		want  string
	}{
		{
			name: "missing cache plain",
			want: "No cached model prices. " + hint + "\n" +
				"claude-sonnet-4-5  no cached price\nnosuchmodel  no cached price\n",
		},
		{
			name:  "empty cache plain",
			cache: `{"host":"owned:any","fetched":0,"models":{}}`,
			want: "No cached model prices. " + hint + "\n" +
				"claude-sonnet-4-5  no cached price\nnosuchmodel  no cached price\n",
		},
		{
			name: "missing cache json",
			json: true,
			want: "{\"cache\":\"empty\",\"hint\":\"" + hint + "\",\"prices\":{}}\n",
		},
		{
			name:  "empty cache json",
			cache: `{"host":"owned:any","fetched":0,"models":{}}`,
			json:  true,
			want:  "{\"cache\":\"empty\",\"hint\":\"" + hint + "\",\"prices\":{}}\n",
		},
		{
			name:  "populated cache miss remains unpriced",
			cache: `{"host":"owned:any","fetched":0,"models":{"priced":{"input":0.1,"output":0.2}}}`,
			args:  []string{"price", "missing"},
			want:  "missing  unpriced\n",
		},
		{
			name:  "zero price remains priced",
			cache: `{"host":"owned:any","fetched":0,"models":{"free":{"input":0,"output":0}}}`,
			args:  []string{"price", "free"},
			want:  "free  $0/$0 per 1M tokens\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_STATE_HOME", root)
			dir := filepath.Join(root, "ccmorph")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.cache != "" {
				if err := os.WriteFile(filepath.Join(dir, "prices-owned-any.json"), []byte(tc.cache), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := tc.args
			if args == nil {
				args = []string{"price", "claude-sonnet-4-5", "nosuchmodel"}
			}
			if tc.json {
				args = append(args, "--json")
			}
			out, rc := captureStdout(t, func() int { return (&launcher{}).verb(args) })
			if rc != 0 || out != tc.want {
				t.Fatalf("rc %d, output %q; want %q", rc, out, tc.want)
			}
		})
	}
}
