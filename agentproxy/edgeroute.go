package agentproxy

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// The Remote Control edge's routing rules that need no request: which words
// name a target, which file names a session's, which edge a handoff is for.
// ccmorph-rc (the launcher and the --switch command, Python) writes every file
// read here; its edge_name, proc_started and edge records are the contract.

var (
	// A switch word: "account", "route" or a profile name.
	// (switchWord is picks.go's: one rule for both.)
	profileName = regexp.MustCompile(`^[A-Za-z0-9_]{1,200}$`)
	notNamePart = regexp.MustCompile(`[^A-Za-z0-9]+`)
	eventsPath  = regexp.MustCompile(`^/v1/code/sessions/[A-Za-z0-9_]+/worker/events$`)
)

// safePart is any token as one file-name part: every run of characters a path
// segment cannot hold becomes a single dash.
func safePart(s string) string { return strings.Trim(notNamePart.ReplaceAllString(s, "-"), "-") }

// edgeName is a route as the file-name part of its edge's record: a valid
// profile name whole (the only route whose edge can have run), any other route
// cleaned and carrying a digest of the whole route so no two share a file.
func edgeName(route string) string {
	if profileName.MatchString(route) {
		return route
	}
	part := strings.Trim(truncate(safePart(route), 64), "-")
	if part == "" {
		part = "route"
	}
	sum := sha256.Sum256([]byte(route))
	return part + "-" + hex.EncodeToString(sum[:])[:32]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// procStarted is when the process at pid started, exactly enough that no later
// process the OS gives the same number can share it, spelled as ccmorph-rc's
// proc_started spells it ("darwin:<sec>.<usec>", "<boot id>:<ticks>").
func procStarted(pid int) (string, bool) {
	tok, ok := util.ProcStartToken(pid)
	if !ok {
		return "", false
	}
	if runtime.GOOS == "darwin" {
		return "darwin:" + tok, true
	}
	return tok, true
}

// startTag is a process start as one file-name part: the first 16 hex digits
// of its sha256, or "unknown" when the start could not be read.
func startTag(started string) string {
	if started == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(started))
	return hex.EncodeToString(sum[:])[:16]
}

// edgeServes is the shared edge a record describes if it can serve right now:
// its process is alive, its proxy answers on loopback and its CA file is still
// there. With route, the record must also be that route's: one naming another
// route is a case twin's, and one naming none counts only when its directory
// lists it spelled exactly as the record's name.
func edgeServes(record, route string) map[string]any {
	b, err := os.ReadFile(record)
	if err != nil {
		return nil
	}
	var edge map[string]any
	if json.Unmarshal(b, &edge) != nil || edge == nil {
		return nil
	}
	if r, ok := edge["route"]; ok {
		if r != route {
			return nil
		}
	} else {
		names, err := os.ReadDir(filepath.Dir(record))
		if err != nil {
			return nil
		}
		found := false
		for _, n := range names {
			found = found || n.Name() == filepath.Base(record)
		}
		if !found {
			return nil
		}
	}
	pid, ok1 := edge["pid"].(float64)
	port, ok2 := edge["port"].(float64)
	ca, ok3 := edge["ca"].(string)
	if !ok1 || !ok2 || !ok3 || syscall.Kill(int(pid), 0) != nil {
		return nil
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	if err != nil {
		return nil
	}
	c.Close()
	if _, err := os.Stat(ca); err != nil {
		return nil
	}
	return edge
}

// routeEdge is route's shared edge under edges if it can serve now.
func routeEdge(edges, route string) map[string]any {
	return edgeServes(filepath.Join(edges, edgeName(route)+".json"), route)
}

// switchEdge is the shared edge `ccmorph-rc <profile> --switch` hands a profile
// to: how many edges under edges serve, and that edge's record with its start
// added, or nil unless exactly one serves and its start can be read.
func switchEdge(edges string) (int, map[string]any) {
	files, _ := filepath.Glob(filepath.Join(paths.GlobQuote(edges), "*.json"))
	sort.Strings(files)
	var live []map[string]any
	for _, f := range files {
		if e := routeEdge(edges, strings.TrimSuffix(filepath.Base(f), ".json")); e != nil {
			live = append(live, e)
		}
	}
	if len(live) != 1 {
		return len(live), nil
	}
	started, ok := procStarted(int(live[0]["pid"].(float64)))
	if !ok {
		return 1, nil
	}
	live[0]["started"] = started
	return 1, live[0]
}

// lostEndpointNote is the per-turn line when the word names profile target and
// this proxy holds no endpoint for it. It names `ccmorph-rc <target> --switch`
// only where that command hands the endpoint to THIS proxy: exactly one shared
// edge serves, and this proxy is that edge's (parent is the pid it follows and
// parentStarted that process's start, the pair a handoff must name). Every
// other case says why nothing can be handed here, never a repair that cannot
// fix it.
func lostEndpointNote(target, bridgeDir string, parent int, session, parentStarted string) string {
	edges := filepath.Join(bridgeDir, "edges")
	how := "ccmorph-rc " + target + " --switch"
	if session != "" {
		how += " --session " + session
	}
	if routeEdge(edges, target) != nil {
		return fmt.Sprintf("switch    %s has its own live edge, so `%s` writes the word 'route', which "+
			"each proxy serves from its own route; on this proxy's route meanwhile", target, how)
	}
	n, edge := switchEdge(edges)
	if edge != nil && int(edge["pid"].(float64)) == parent {
		switch {
		case edge["started"] == parentStarted && parentStarted != "":
			return fmt.Sprintf("switch    %s has no endpoint in this proxy; `%s` hands it over; on the route meanwhile", target, how)
		case parentStarted == "":
			return fmt.Sprintf("switch    %s has no endpoint in this proxy, which predates handoffs naming their "+
				"edge's start, so a switch can hand it nothing until edge %d restarts; on the route", target, parent)
		default:
			return fmt.Sprintf("switch    %s has no endpoint in this proxy, which follows an earlier process at "+
				"pid %d than the live shared edge: a switch hands profiles to that edge's own "+
				"proxy only; on the route", target, parent)
		}
	}
	if edge != nil {
		return fmt.Sprintf("switch    %s has no endpoint in this proxy, which is not the shared edge's: "+
			"a switch hands profiles to edge %d only; on the route", target, int(edge["pid"].(float64)))
	}
	if n == 1 {
		return fmt.Sprintf("switch    %s has no endpoint in this proxy, and the start of the one live shared "+
			"edge cannot be read here, so nothing can be handed to it; on the route", target)
	}
	return fmt.Sprintf("switch    %s has no endpoint in this proxy and no single live shared edge to hand "+
		"it to (%d live); on the route", target, n)
}

// inferenceTarget is where inference goes for a request: "account" sends it to
// Anthropic on the session's own claude.ai login, untouched; "route" (also a
// missing, unreadable or malformed file) to the edge's own route; any other
// word is a ccmorph profile `ccmorph-rc <profile> --switch` brought up beside
// it. Read on every inference request, so a switch takes effect on the next
// turn with no restart.
func inferenceTarget(switchFile string) string {
	if switchFile == "" {
		return "route"
	}
	b, err := os.ReadFile(switchFile)
	if err != nil {
		return "route"
	}
	if w := strings.TrimSpace(string(b)); switchWord.MatchString(w) {
		return w
	}
	return "route"
}

// Claude Code stamps every request with the session it belongs to, the same
// for its subagents. A Remote Control session child is started with
// `--sdk-url https://api.anthropic.com/v1/code/sessions/<cse_...>`, and Claude
// Code names its transcript, and so this header, by a UUIDv5 of that URL in
// its own namespace; the header is never the cse_ id the phone and
// `--switch --session` know.
const sdkSessionURL = "https://api.anthropic.com/v1/code/sessions/"

var transcriptNamespace = []byte{0x3a, 0xb1, 0x9d, 0x7e, 0x9f, 0x35, 0x45, 0xc2, 0x92, 0x6e, 0x75, 0xe2, 0x71, 0xcc, 0x60, 0xb3}

// headerID is the X-Claude-Code-Session-Id a Remote Control session cse sends.
func headerID(cse string) string {
	sum := sha1.Sum(append(append([]byte{}, transcriptNamespace...), sdkSessionURL+cse...))
	d := sum[:16]
	d[6] = d[6]&0x0f | 0x50
	d[8] = d[8]&0x3f | 0x80
	h := hex.EncodeToString(d)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// sessionTarget is the target `ccmorph-rc <profile> --switch --session <id>`
// chose for this one session ("account", "route" or a profile), else "": the
// session follows the switch every host shares. session is the request's
// header; a file is named by the cse_ id, so the header is matched against
// each file's derived id. Read on every inference request, like the switch.
func sessionTarget(sessions, session string) string {
	if sessions == "" || !sessionID.MatchString(session) {
		return ""
	}
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if n := e.Name(); n == session || headerID(n) == session {
			b, err := os.ReadFile(filepath.Join(sessions, n))
			if err != nil {
				continue
			}
			if w := strings.TrimSpace(string(b)); switchWord.MatchString(w) {
				return w
			}
			return ""
		}
	}
	return ""
}

func isFirstPartyHost(host string) bool {
	return strings.EqualFold(strings.TrimSuffix(host, "."), anthropicHost)
}

// isEventUpload is whether a request is the host's upload of session events.
func isEventUpload(host, path string) bool {
	return isFirstPartyHost(host) && eventsPath.MatchString(path)
}
