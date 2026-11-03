package util

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
)

var (
	taskTag    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	attemptTag = regexp.MustCompile(`^[0-9]{1,3}$`)
)

// LaunchEnv is the profile's launch env pointed at the local bridge (base and key,
// added when missing) or at the adapter (base only), plus the task tags the gateway
// shim records when their shapes are valid on an adapter launch. Entries come back
// in order. An empty bridgePort means no bridge.
//
// The bridge key is never an operand of anything: a credential on a command line
// is visible to anything running as this user (.github/SECURITY.md).
func LaunchEnv(env []string, bridgePort, bridgeKey string, adapter bool, adapterPort, task, attempt string) []string {
	env = append([]string{}, env...)
	set := func(key, val string, add bool) {
		for i, e := range env {
			if strings.HasPrefix(e, key+"=") {
				env[i] = key + "=" + val
				return
			}
		}
		if add {
			env = append(env, key+"="+val)
		}
	}
	if bridgePort != "" {
		set("ANTHROPIC_BASE_URL", "http://127.0.0.1:"+bridgePort, true)
		set("ANTHROPIC_API_KEY", bridgeKey, true)
	}
	if adapter && adapterPort != "" {
		// Only what Claude Code sees; the profile keeps its real base_url.
		set("ANTHROPIC_BASE_URL", "http://127.0.0.1:"+adapterPort, false)
	}
	var hdrs []string
	if taskTag.MatchString(task) {
		hdrs = append(hdrs, "x-ccmorph-task: "+task)
	}
	if attemptTag.MatchString(attempt) {
		hdrs = append(hdrs, "x-ccmorph-attempt: "+attempt)
	}
	if adapter && len(hdrs) > 0 {
		env = append(env, "ANTHROPIC_CUSTOM_HEADERS="+strings.Join(hdrs, "\n"))
	}
	return env
}

// launchEnv is `util launch-env <bridge port> <adapter 0|1> <adapter port> <task> <attempt>`
// with the profile's launch env on stdin, one KEY=value per line: LaunchEnv as text,
// NUL-joined (a header value spans lines), closed by a dot. The bridge key arrives
// in _CCM_BRIDGE_KEY; a v0.4.3 launcher still running after an update sends it as
// the second operand instead, and that form is read (#137,
// tests/old_launcher_contract_test.py).
func launchEnv(args []string, in io.Reader, out, _ io.Writer) int {
	bridgeKey := os.Getenv("_CCM_BRIDGE_KEY")
	if len(args) == 6 {
		bridgeKey, args = args[1], append([]string{args[0]}, args[2:]...)
	}
	if len(args) != 5 {
		return 2
	}
	var env []string
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if sc.Text() != "" {
			env = append(env, sc.Text())
		}
	}
	for _, e := range LaunchEnv(env, args[0], bridgeKey, args[1] == "1", args[2], args[3], args[4]) {
		io.WriteString(out, e+"\x00")
	}
	io.WriteString(out, ".")
	return 0
}
