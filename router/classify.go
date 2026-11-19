package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxChars = 7000

var kindCriteria = map[string]string{
	"writing":    "Write, rewrite, summarize or translate text.",
	"search":     "Find live information on the web.",
	"x":          "Find posts on X (Twitter).",
	"mechanical": "Read files, run checks, rename or make routine edits.",
	"code":       "Implement, debug, review or reason about code.",
	"design":     "Design a product, interface or visual direction.",
	"image_gen":  "Generate or edit an image, not just inspect one.",
	"vision":     "Inspect images, screenshots, scanned PDFs or video.",
}

var tierCriteria = map[string]string{
	"small": "Routine, bounded work with a clear answer or procedure.",
	"mid":   "Everyday work that needs reasoning and implementation.",
	"top":   "Hard, ambiguous or high-stakes work that needs the strongest model.",
}

// Decider is an optional typed-decision endpoint: it takes {state, questions}
// and answers {answers: {name: {choice|noul: ...}}}. Without one, keyword rules decide.
type Decider struct {
	URL    string `json:"url"`
	Model  string `json:"model,omitempty"`
	KeyEnv string `json:"key_env,omitempty"`
}

type answer struct {
	Choice string   `json:"choice"`
	Noul   *float64 `json:"noul"`
}

// ask sends one decision request. No proxy and no redirect: a key never follows a 3xx.
func (d *Decider) ask(state, questions map[string]any, timeout time.Duration) (map[string]answer, error) {
	if d == nil || d.URL == "" {
		return nil, errors.New("no decider")
	}
	payload := map[string]any{"state": state, "questions": questions}
	if d.Model != "" {
		payload["model"] = d.Model
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", d.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if d.KeyEnv != "" {
		key := os.Getenv(d.KeyEnv)
		if key == "" {
			return nil, errors.New("decider key is not set")
		}
		req.Header.Set("authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("decider answered " + resp.Status)
	}
	var out struct {
		Answers map[string]answer `json:"answers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Answers == nil {
		return nil, errors.New("decider returned invalid answers")
	}
	return out.Answers, nil
}

func choice(instructions string, criteria map[string]string) map[string]any {
	return map[string]any{"type": "choice", "instructions": instructions, "criteria": criteria}
}

func clipped(s string) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars/2] + "\n...\n" + s[len(s)-maxChars/2:]
}

type rule struct {
	kind string
	re   *regexp.Regexp
}

var kindRules = []rule{
	{"image_gen", regexp.MustCompile(`(?s)\b(?:generate|create|draw|render|edit)\b.{0,60}\b(?:image|picture|illustration|logo|artwork)\b`)},
	{"x", regexp.MustCompile(`\b(twitter|tweets?|x\.com)\b|\bsearch x\b|\bposts? (on|from) x\b`)},
	{"vision", regexp.MustCompile(`\b(screenshot|image|photo|picture|diagram|pdf|video)\b`)},
	{"search", regexp.MustCompile(`\b(search|research|look up|find out|latest|news|survey|browse|reddit)\b`)},
	{"design", regexp.MustCompile(`\b(design|figma|mockup|interface|layout|ui|ux)\b`)},
	{"mechanical", regexp.MustCompile(`\b(rename|bulk|every file|all files|grep|list|format|move|copy|bump)\b`)},
	{"code", regexp.MustCompile(`\b(fix|implement|refactor|debug|audit|review|bug|test|function|class|endpoint|code|architecture)\b`)},
	{"writing", regexp.MustCompile(`\b(write|draft|rewrite|email|blog|readme|report|summari[sz]e|translate)\b`)},
}

var (
	hardWords = regexp.MustCompile(`\b(architecture|concurrency|race|security|migration|distributed|optimization|prove|from scratch)\b`)
	easyWords = regexp.MustCompile(`\b(rename|typo|list|bump|format|one line|small|quick|simple|grep)\b`)
)

// Heuristic is the keyword fallback when no decider answers.
func Heuristic(prompt string) Task {
	text := strings.ToLower(prompt)
	t := Task{Kind: "code", Tier: "mid", Source: "heuristic"}
	for _, r := range kindRules {
		if r.re.MatchString(text) {
			t.Kind = r.kind
			break
		}
	}
	if hardWords.MatchString(text) {
		t.Tier = "top"
	} else if easyWords.MatchString(text) {
		t.Tier = "small"
	}
	return t
}

// Classify asks the decider for kind and tier, or falls back to keyword rules with a note.
func Classify(prompt string, d *Decider, timeout time.Duration) (Task, string) {
	if d == nil || d.URL == "" {
		return Heuristic(prompt), ""
	}
	a, err := d.ask(map[string]any{"task": clipped(prompt)}, map[string]any{
		"kind": choice("What kind of work is this task mainly?", kindCriteria),
		"tier": choice("Which model tier does this task need?", tierCriteria),
	}, timeout)
	if err == nil && slices.Contains(Kinds, a["kind"].Choice) && Efforts[a["tier"].Choice] != "" {
		return Task{Kind: a["kind"].Choice, Tier: a["tier"].Choice, Source: "decider"}, ""
	}
	return Heuristic(prompt), "decider unavailable; used keyword rules"
}

var (
	tokenSplit = regexp.MustCompile(`[-_/.\s]+`)
	tierWords  = []string{"haiku", "sonnet", "opus", "luna", "terra", "sol", "mini", "nano", "flash", "lite", "small",
		"pro", "ultra", "max", "argon", "reasoner"}
	knownFamilies = []string{"claude", "gpt", "gemini", "grok", "qwen", "deepseek", "glm", "kimi"}
)

func tokens(model string) []string {
	return tokenSplit.Split(strings.ToLower(strings.TrimSuffix(model, "[1m]")), -1)
}

// namedTier: a vendor tier word in the ID is the vendor's own label.
func namedTier(model string) bool {
	for _, t := range tokens(model) {
		if slices.Contains(tierWords, t) {
			return true
		}
	}
	return false
}

// HeuristicLabel reads family and tier from the served ID's own words.
func HeuristicLabel(model string) Label {
	toks := tokens(model)
	has := func(w ...string) bool {
		for _, t := range toks {
			if slices.Contains(w, t) {
				return true
			}
		}
		return false
	}
	for fam, aliases := range map[string][3]string{"claude": {"haiku", "sonnet", "opus"}, "gpt": {"luna", "terra", "sol"}} {
		for i, a := range aliases {
			if has(a) {
				return Label{Family: fam, Tier: Tiers[i]}
			}
		}
	}
	family := ""
	for _, f := range knownFamilies {
		for _, t := range toks {
			if strings.HasPrefix(t, f) {
				family = f
			}
		}
		if family != "" {
			break
		}
	}
	if family == "gemini" && has("flash") {
		family = "gemini-flash"
	}
	if family == "" {
		name := strings.ToLower(strings.TrimSuffix(model, "[1m]"))
		name = name[strings.LastIndex(name, "/")+1:]
		family = regexp.MustCompile(`[-_.]`).Split(name, 2)[0]
		if family == "" {
			family = "unknown"
		}
	}
	tier := "mid"
	if has("mini", "nano", "flash", "lite", "small") {
		tier = "small"
	} else if has("pro", "ultra", "max", "argon", "reasoner", "top") {
		tier = "top"
	}
	return Label{Family: family, Tier: tier}
}

// labelModel labels an unseen served ID once and caches it: the decider when the
// ID names no tier, the ID's own words otherwise.
func labelModel(model string, d *Decider, timeout time.Duration) Label {
	sum := sha256.Sum256([]byte(model))
	path := filepath.Join(routerDir(), "labels", hex.EncodeToString(sum[:])+".json")
	var hit Label
	if readJSON(path, &hit) && hit.Family != "" && Efforts[hit.Tier] != "" {
		return hit
	}
	l := HeuristicLabel(model)
	if d != nil && d.URL != "" && !namedTier(model) && timeout > 0 {
		criteria := map[string]string{l.Family: l.Family}
		for _, f := range append(knownFamilies, "gemini-flash") {
			criteria[f] = f
		}
		a, err := d.ask(map[string]any{"model": model}, map[string]any{
			"family": choice("Which model family does this served ID belong to?", criteria),
			"tier":   choice("Which tier is this model within its family?", tierCriteria),
		}, timeout)
		if err == nil && criteria[a["family"].Choice] != "" && Efforts[a["tier"].Choice] != "" {
			l = Label{Family: a["family"].Choice, Tier: a["tier"].Choice}
		}
	}
	_ = writeJSON(path, l)
	return l
}

// checkResult asks the decider whether the answer did the brief; nil when it cannot say.
func checkResult(d *Decider, brief, result string) *bool {
	a, err := d.ask(map[string]any{"brief": clipped(brief), "result": clipped(result)}, map[string]any{
		"pass": map[string]any{"type": "noul", "instructions": "Did the result do the brief? Pass if it did; fail if it did not."},
	}, 3*time.Second)
	if err != nil || a["pass"].Noul == nil {
		return nil
	}
	v := *a["pass"].Noul
	if math.IsNaN(v) || v < 0 || v > 1 {
		return nil
	}
	pass := v >= 0.5
	return &pass
}
