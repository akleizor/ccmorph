package util

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

var (
	taskTag    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	attemptTag = regexp.MustCompile(`^[0-9]{1,3}$`)
)

// launchEnv is `util launch-env <bridge port> <bridge key> <adapter 0|1> <adapter port> <task> <attempt>`
// with the profile's launch env on stdin, one KEY=value per line. It points Claude
// Code at the local bridge (base and key, added when missing) or at the adapter
// (base only), and adds the task tags the gateway shim records when their shapes
// are valid on an adapter launch. The env goes back out in order, NUL-joined
// (a header value spans lines), closed by a dot.
func launchEnv(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) != 6 {
		return 2
	}
	bridgePort, bridgeKey, adapter, adapterPort, task, attempt := args[0], args[1], args[2] == "1", args[3], args[4], args[5]
	var env []string
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if sc.Text() != "" {
			env = append(env, sc.Text())
		}
	}
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
	for _, e := range env {
		io.WriteString(out, e+"\x00")
	}
	io.WriteString(out, ".")
	return 0
}
