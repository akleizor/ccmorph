package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if rc != 0 || out != "{\"cache\":\"ok\",\"prices\":{\"model-FP8\":{\"input\":0.15,\"output\":0.6}}}\n" {
		t.Fatalf("rc %d, output %q", rc, out)
	}
}

func TestPriceEmptyCache(t *testing.T) {
	const hint = priceCacheHint
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

// #636: an option-shaped argument was read as a model id, so `--jsoon` printed
// an unpriced line with exit 0; after --, a dash-leading id is a model.
func TestPriceRefusesAnUnknownOption(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := filepath.Join(root, "ccmorph")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"host":"owned:any","fetched":0,"models":{"-x":{"input":1,"output":2}}}`
	if err := os.WriteFile(filepath.Join(dir, "prices-owned-any.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"price", "--jsoon", "x"}, {"price", "x", "-j"}} {
		out, rc := captureStdout(t, func() int { return (&launcher{}).verb(args) })
		if rc != 2 || out != "" {
			t.Errorf("%v: rc %d, stdout %q; want rc 2 and nothing priced", args, rc, out)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"price", "--", "-x"}, "-x  $1/$2 per 1M tokens\n"},
		{[]string{"price", "--json", "--", "--json"}, "{\"cache\":\"ok\",\"prices\":{}}\n"},
	} {
		out, rc := captureStdout(t, func() int { return (&launcher{}).verb(c.args) })
		if rc != 0 || out != c.want {
			t.Errorf("%v: rc %d, stdout %q, want %q", c.args, rc, out, c.want)
		}
	}
}
