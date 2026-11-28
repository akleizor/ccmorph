package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

func dupConfLauncher(t *testing.T) (*launcher, string) {
	root := t.TempDir()
	state, profiles, home := filepath.Join(root, "state"), filepath.Join(root, "profiles"), filepath.Join(root, "home")
	for _, d := range []string{state, filepath.Join(profiles, "gw"), filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	conf := filepath.Join(state, "gw.conf")
	body := "type=apikey\nbase_url=https://gw.test\napi_key=k\nmodel=m\nmodel=m\n\nmodel=m\nmodel=m\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &launcher{dir: state, profileDirs: profiles}, conf
}

func TestListDescribesAProfileWhoseModelLineIsRepeatedIssue1031(t *testing.T) {
	l, _ := dupConfLauncher(t)
	descs, skipped := l.describeProfiles("", nil, nil)
	if len(descs) != 1 || skipped != 0 {
		t.Fatalf("descs %d skipped %d", len(descs), skipped)
	}
}

func TestLaunchSequenceLeavesARepeatedModelProfileReadableIssue1031(t *testing.T) {
	l, conf := dupConfLauncher(t)
	l.mapping = profile.MappingEnv
	if _, ok := l.read("gw"); !ok {
		t.Fatal("read refused")
	}
	if out, rc := l.configStore("seed-model-defaults", "gw", "model"); rc != 0 || strings.TrimSpace(out) != "m" {
		t.Fatalf("rc %d out %q", rc, out)
	}
	b, _ := os.ReadFile(conf)
	if n := strings.Count(string(b), "model=m\n"); n != 1 {
		t.Fatalf("model lines %d:\n%s", n, b)
	}
	if _, ok := l.read("gw"); !ok {
		t.Fatal("read refused")
	}
}
