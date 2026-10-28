package settingscompose

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/pycompat"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	canonical, profile, shared string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{filepath.Join(dir, "claude", "settings.json"), filepath.Join(dir, "profile"), filepath.Join(dir, "policy.json")}
	write(t, f.canonical, "{}")
	if err := os.MkdirAll(f.profile, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) run(command string) (int, string) {
	var stderr bytes.Buffer
	args := []string{command, f.profile, "--canonical", f.canonical}
	if command == "materialize" {
		args = append(args, "--saved", "{}")
	}
	code := Main(args, &stderr)
	return code, stderr.String()
}

func (f fixture) linkPolicy(t *testing.T, policy string) {
	t.Helper()
	write(t, f.shared, policy)
	if err := os.Symlink(f.shared, filepath.Join(f.profile, policyFile)); err != nil {
		t.Fatal(err)
	}
}

const sandbox = `{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/Documents"]}}}`

func settingsBytes(t *testing.T, f fixture) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Python: test_a_policy_read_failure_preserves_the_generated_sandbox (issues 308, 318).
func TestPolicyReadFailurePreservesTheGeneratedSandbox(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		replacement bool
	}{
		{"disappearing-target", fs.ErrNotExist, false},
		{"unreadable-target", &fs.PathError{Op: "open", Err: syscall.EACCES}, false},
		{"replaced-policy", fs.ErrNotExist, true},
		{"too-deep-target", pycompat.DepthError{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.linkPolicy(t, sandbox)
			if code, stderr := f.run("materialize"); code != 0 {
				t.Fatalf("first projection: %d %s", code, stderr)
			}
			before := settingsBytes(t, f)
			policy := filepath.Join(f.profile, policyFile)
			real := readFile
			readFile = func(path string) ([]byte, error) {
				if path == policy {
					if c.replacement {
						if err := os.Rename(f.shared, policy); err != nil {
							t.Fatal(err)
						}
					}
					return nil, c.err
				}
				return real(path)
			}
			defer func() { readFile = real }()
			code, stderr := f.run("materialize")
			if code != 3 || !strings.Contains(stderr, "settings.policy.json") {
				t.Fatalf("want exit 3 naming the policy, got %d %q", code, stderr)
			}
			if !bytes.Equal(settingsBytes(t, f), before) {
				t.Fatal("settings.json changed")
			}
			if !strings.Contains(string(before), `"denyRead"`) {
				t.Fatal("the generated settings lost the sandbox")
			}
		})
	}
}

// Python: test_a_policy_sharing_canonicals_deny_list_still_counts_as_carried.
func TestPolicySharingCanonicalsDenyListCountsAsCarried(t *testing.T) {
	f := newFixture(t)
	write(t, f.canonical, `{"permissions":{"deny":["Bash(shutdown*)"]}}`)
	f.linkPolicy(t, `{"permissions":{"deny":["Read(~/.ssh/**)"]}}`)
	if code, stderr := f.run("materialize"); code != 0 {
		t.Fatal(code, stderr)
	}
	policy := readPolicy(f.profile)
	if !carriesPolicy(f.profile, policy) {
		t.Fatal("policy deny rule is in settings.json but was not counted as carried")
	}
}

// Python: test_a_crash_while_deciding_the_fallback_refuses_issue_420.
func TestACrashWhileDecidingTheFallbackRefuses(t *testing.T) {
	f := newFixture(t)
	f.linkPolicy(t, sandbox)
	if code, stderr := f.run("materialize"); code != 0 {
		t.Fatal(code, stderr)
	}
	before := settingsBytes(t, f)
	lock := filepath.Join(f.profile, lockFile)
	os.Remove(lock)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	real := readPolicyFn
	readPolicyFn = func(dir string) *obj { panic(errors.New("cannot hold the policy")) }
	defer func() { readPolicyFn = real }()
	code, stderr := f.run("materialize")
	if code != 3 || !strings.Contains(stderr, "settings.policy.json") {
		t.Fatalf("want exit 3, got %d %q", code, stderr)
	}
	if !bytes.Equal(settingsBytes(t, f), before) {
		t.Fatal("settings.json changed")
	}
}

func TestMalformedTombstonesAreACrashNamedTypeError(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.profile, layerFile), `{"$ccmorphUnset": null}`)
	code, stderr := f.run("materialize")
	if code != 1 || !strings.Contains(stderr, "TypeError") {
		t.Fatalf("got %d %q", code, stderr)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"materialize"}, {"fold", "p", "--bogus"}} {
		var stderr bytes.Buffer
		if code := Main(args, &stderr); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestFoldKeepsClaudeEditsInTheLayer(t *testing.T) {
	f := newFixture(t)
	write(t, f.canonical, `{"theme":"dark"}`)
	if code, stderr := f.run("materialize"); code != 0 {
		t.Fatal(code, stderr)
	}
	write(t, filepath.Join(f.profile, "settings.json"), `{"theme":"dark","verbose":true}`)
	if code, stderr := f.run("fold"); code != 0 {
		t.Fatal(code, stderr)
	}
	layer, _ := os.ReadFile(filepath.Join(f.profile, layerFile))
	if got, want := string(layer), "{\n  \"verbose\": true\n}\n"; got != want {
		t.Fatalf("layer = %q, want %q", layer, want)
	}
}
