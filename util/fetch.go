package util

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// fetchListing is `util fetch-listing <base> <launch|models|doctor> [max seconds]`
// with the credential as stdin's first line: GET <base>/v1/models?limit=1000 with
// x-api-key and Bearer headers, the body undisguised on stdout.
//
//   - launch: the per-launch listing; sends anthropic-version, fails on non-2xx.
//   - models: `ccmorph models` and model-known; no anthropic-version (proxies
//     disguise non-Claude ids when they see it), fails on non-2xx.
//   - doctor: never fails; the body is followed by "\n<http status>", 000 when
//     nothing answered.
//
// The ambient HTTPS_PROXY is honoured: it is how a bridge reaches a private endpoint.
func fetchListing(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) < 2 {
		return 2
	}
	base, mode, maxTime := args[0], args[1], "20"
	if len(args) > 2 {
		maxTime = args[2]
	}
	key, _ := bufio.NewReader(in).ReadString('\n')
	key = strings.TrimRight(key, "\r\n")
	u := apiURL(base, "/v1/models?limit=1000")
	var curlArgs []string
	switch mode {
	case "launch":
		curlArgs = []string{"-sS", "-f", "--connect-timeout", "2", "--max-time", maxTime, u, "-H", "anthropic-version: 2023-06-01"}
	case "models":
		curlArgs = []string{"-sS", "-f", "--max-time", maxTime, u}
	case "doctor":
		curlArgs = []string{"-sS", "--connect-timeout", "2", "--max-time", "5", "-w", `\n__CCMORPH_STATUS__%{http_code}`, u}
	default:
		return 2
	}
	body, err := curlKeyed(key, curlArgs)
	if mode == "doctor" {
		code := "000"
		if i := strings.LastIndex(body, "__CCMORPH_STATUS__"); err == nil && i >= 0 {
			body, code = strings.TrimSuffix(body[:i], "\n"), body[i+len("__CCMORPH_STATUS__"):]
		} else {
			body = ""
		}
		fmt.Fprintf(out, "%s\n%s", undisguise(body), code)
		return 0
	}
	if err != nil || body == "" {
		return 1
	}
	io.WriteString(out, undisguise(body))
	return 0
}

// curlKeyed runs curl with the credential headers fed through its stdin config
// (-K -), so the key never appears on a command line.
// shortcut: curl is the transport so the gateway test stubs keep answering; move to
// net/http when those fixtures become a loopback server.
func curlKeyed(key string, args []string) (string, error) {
	esc := strings.ReplaceAll(strings.ReplaceAll(key, `\`, `\\`), `"`, `\"`)
	cmd := exec.Command("curl", append([]string{"-K", "-"}, args...)...)
	cmd.Stdin = strings.NewReader("header = \"x-api-key: " + esc + "\"\nheader = \"Authorization: Bearer " + esc + "\"\n")
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	return string(b), err
}

// apiURL joins an API root or a versioned SDK base (".../v1") with a /v1 path.
func apiURL(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if versioned.MatchString(strings.ToLower(base)) {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

var (
	versioned      = regexp.MustCompile(`/v[0-9]+$`)
	quotedDisguise = regexp.MustCompile(`"(claude-[^"]*-dd-[^"]+)"`)
)

// undisguise restores every quoted claude-<cover>-dd-<reversed> id to its real id.
func undisguise(body string) string {
	return quotedDisguise.ReplaceAllStringFunc(body, func(q string) string {
		return `"` + ctxwindow.RealID(q[1:len(q)-1]) + `"`
	})
}
