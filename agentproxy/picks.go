package agentproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/akleizor/ccmorph/util/paths"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The pick protocol the router's PreToolUse hook and a proxy share: the hook
// writes {word, model} to <dir>/pending/<sha256 of the agent's prompt>.json;
// the proxy binds it to the agent whose first request carries that prompt by
// renaming it to <dir>/<agent id>.json; SubagentStop removes the binding. The
// Remote Control proxy (rc_route.claim_agent) keeps the same pick shape and
// prompt key in one pending folder per session.

// AgentHeader names the subagent a request comes from.
const AgentHeader = "x-claude-code-agent-id"

// DirEnv names the pick dir of the agent proxy a session runs behind.
const DirEnv = "CCMORPH_AGENT_PROXY"

var (
	agentID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	switchWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

const (
	pendingTTL = 600 * time.Second  // a pick waits this long for its agent's first request
	boundTTL   = 7 * 24 * time.Hour // a binding whose agent never stopped is swept after this
)

// Pick is where one subagent's requests go: a switch word (a profile name for
// this proxy) and the model to serve, "" to keep the request's own.
type Pick struct {
	Word  string `json:"word"`
	Model string `json:"model,omitempty"`
}

// PromptKey names a pending pick: sha256 of the stripped prompt.
func PromptKey(prompt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(prompt)))
	return hex.EncodeToString(sum[:])
}

// LeavePick leaves pick in dir for the agent whose prompt this is.
func LeavePick(dir, prompt string, p Pick) error {
	pending := filepath.Join(dir, "pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"word": p.Word, "model": p.Model})
	return paths.AtomicWrite(paths.WriteIn{Path: filepath.Join(pending, PromptKey(prompt)+".json"), Temp: ".pick.*.tmp", Data: b, NoChmod: true})
}

// DropAgent removes a stopped agent's binding.
func DropAgent(dir, agent string) {
	if dir != "" && agentID.MatchString(agent) {
		os.Remove(filepath.Join(dir, agent+".json"))
	}
}

func readPick(path string) (Pick, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Pick{}, false
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return Pick{}, false
	}
	word, _ := raw["word"].(string)
	if !switchWord.MatchString(word) {
		return Pick{}, false
	}
	model, _ := raw["model"].(string)
	return Pick{Word: word, Model: model}, true
}

// Bound is the pick bound to agent.
func Bound(dir, agent string) (Pick, bool) {
	if dir == "" || !agentID.MatchString(agent) {
		return Pick{}, false
	}
	return readPick(filepath.Join(dir, agent+".json"))
}

// HasPending is whether any pick waits for its agent: only then is a new
// agent's body read before it is sent.
func HasPending(dir string) bool {
	if dir == "" {
		return false
	}
	names, _ := filepath.Glob(filepath.Join(dir, "pending", "*.json"))
	return len(names) > 0
}

// Claim binds agent to the pick its prompt names, read from the first message
// of the request body. A rename takes the pick, so one pick binds one agent.
// Stale picks and bindings are swept here.
func Claim(dir, agent string, body []byte, now time.Time) (Pick, bool) {
	if dir == "" || !agentID.MatchString(agent) {
		return Pick{}, false
	}
	pending := filepath.Join(dir, "pending")
	sweep(pending, now.Add(-pendingTTL))
	sweep(dir, now.Add(-boundTTL))
	var payload struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Messages) == 0 {
		return Pick{}, false
	}
	var texts []string
	var s string
	var blocks []struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(payload.Messages[0].Content, &s) == nil {
		texts = []string{s}
	} else if json.Unmarshal(payload.Messages[0].Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Text != nil {
				texts = append(texts, *b.Text)
			}
		}
	}
	bound := filepath.Join(dir, agent+".json")
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if os.Rename(filepath.Join(pending, PromptKey(t)+".json"), bound) == nil {
			return readPick(bound)
		}
	}
	return Pick{}, false
}

func sweep(dir string, before time.Time) {
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, n := range names {
		if fi, err := os.Stat(n); err == nil && fi.ModTime().Before(before) {
			os.Remove(n)
		}
	}
}
