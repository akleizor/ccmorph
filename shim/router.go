package shim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The turn router is one per-profile opt-in: env.CCMORPH_TURN_ROUTER names an
// absolute path plus arguments, and at the start of each user turn the shim runs
// it once with the turn's facts on stdin. It answers {"model": "<id>"} to switch
// this turn's model or {} to keep the asked one. Nothing here runs when the
// setting is absent, so the shim's requests stay byte-identical (promise 4 is
// kept by consent, and the switch is then announced — see noticeStream).

const (
	routerTimeout = 3 * time.Second
	routerMaxOut  = 64 << 10
	routerThreads = 1024
)

// notATurn open a user-role message that is not a human turn: a background
// task's completion notice, and the two shapes Claude Code's compaction uses.
var notATurn = []string{
	"<task-notification>",
	"This session is being continued from a previous conversation",
	"Your task is to create a detailed summary of the conversation so far",
}

// userTurn is one request's place in a conversation: the thread it belongs to
// (session plus the conversation's opening text, so a subagent never inherits
// the main loop's decision) and the new human text, when this request begins one.
type userTurn struct {
	thread string
	prompt string
	count  int // messages this request carries: a retry has the same count, a new turn more
}

// readTurn locates a user turn in body. ok is false for any request the router
// must not see: one that carries no tools (Claude Code's side calls — title,
// classifier), no messages, or a body it cannot read.
func readTurn(body []byte, session string) (userTurn, bool) {
	var req struct {
		Tools    []jsontext.Value `json:"tools"`
		Messages []struct {
			Role    string         `json:"role"`
			Content jsontext.Value `json:"content"`
		} `json:"messages"`
	}
	if err := jsonv2.Unmarshal(body, &req, jsontext.AllowDuplicateNames(true)); err != nil ||
		len(req.Tools) == 0 || len(req.Messages) == 0 {
		return userTurn{}, false
	}
	first := userText(req.Messages[0].Content)
	sum := sha256.Sum256([]byte(first))
	t := userTurn{thread: session + "\x00" + hex.EncodeToString(sum[:]), count: len(req.Messages)}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return t, true
	}
	t.prompt = strings.TrimSpace(userText(last.Content))
	for _, prefix := range notATurn {
		if strings.HasPrefix(t.prompt, prefix) {
			t.prompt = ""
			break
		}
	}
	return t, true
}

// userText is a message's human text: the string itself, or its text blocks
// joined — never tool results, and never Claude Code's injected reminders.
func userText(content jsontext.Value) string {
	if len(content) == 0 {
		return ""
	}
	if content[0] == '"' {
		var s string
		if jsonv2.Unmarshal(content, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if jsonv2.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<system-reminder>") {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TurnRouter is the configured router command. Nil is off: the shim then has no
// turn boundary, no ask, and no rewrite.
type TurnRouter struct {
	argv    []string
	timeout time.Duration
	run     func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)
	logf    func(format string, args ...any)

	mu      sync.Mutex
	threads map[string]*turnState
}

// turnState is one thread's decision: the human text that asked for it, the
// model the request asked for, and the model the router chose ("" keeps the
// asked one). notice stays set until the turn's first answer carries it.
type turnState struct {
	prompt [32]byte
	count  int
	asked  string
	model  string
	notice bool
}

// NewTurnRouter reads CCMORPH_TURN_ROUTER: an absolute path plus arguments. An
// unusable setting fails the launch with its reason, never a silent pass-through.
func NewTurnRouter(spec string) (*TurnRouter, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	argv := strings.Fields(spec)
	if !filepath.IsAbs(argv[0]) {
		return nil, fmt.Errorf("CCMORPH_TURN_ROUTER must start with an absolute path, got %q", argv[0])
	}
	st, err := os.Stat(argv[0])
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("CCMORPH_TURN_ROUTER is not an executable file: %s", argv[0])
	}
	return &TurnRouter{
		argv: argv, timeout: routerTimeout, run: runRouter,
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "shim: turn router: "+format+"\n", args...)
		},
		threads: map[string]*turnState{},
	}, nil
}

// runRouter runs the command once. It hands it a minimal environment: PATH to
// find its own tools, and neither of the shim's credentials (promise 2).
func runRouter(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var env []string
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: routerMaxOut}
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// limitedWriter fails a router that answers more than n bytes.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("answer too large")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// route is the model to send for this request, the notice its answer carries
// while the turn's first answer is still unsent, and the thread it belongs to.
// It never fails: a router fault keeps the asked model.
func (r *TurnRouter) route(t userTurn, session, current string, contextTokens int, models func() []string) (model, notice, thread string) {
	thread = t.thread
	sum := sha256.Sum256([]byte(t.prompt))
	r.mu.Lock()
	st := r.threads[thread]
	// A new human turn either opens the thread or adds messages; a retry of the
	// same request carries the same prompt and count and reuses the decision.
	ask := t.prompt != "" && (st == nil || sum != st.prompt || t.count > st.count)
	r.mu.Unlock()
	if ask {
		st = r.ask(t, sum, session, current, contextTokens, models)
		r.mu.Lock()
		if len(r.threads) >= routerThreads {
			r.threads = map[string]*turnState{}
		}
		r.threads[thread] = st
		r.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.notice {
		return st.model, noticeText(st.asked, st.model), thread
	}
	return st.model, "", thread
}

// ask runs the command once and decides the model. Every failure keeps current
// and says why in the shim's log; none of them fails the request.
func (r *TurnRouter) ask(t userTurn, sum [32]byte, session, current string, contextTokens int, models func() []string) *turnState {
	st := &turnState{prompt: sum, count: t.count, asked: current}
	served := models()
	in, err := jsonv2.Marshal(struct {
		Session       string   `json:"session"`
		CurrentModel  string   `json:"current_model"`
		Prompt        string   `json:"prompt"`
		ContextTokens int      `json:"context_tokens"`
		Models        []string `json:"models"`
	}{session, current, t.prompt, contextTokens, served})
	if err != nil {
		r.logf("cannot encode the ask: %v; keeping %s", err, current)
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	out, err := r.run(ctx, r.argv, in)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.logf("timed out after %.0fs; keeping %s", r.timeout.Seconds(), current)
		return st
	case err != nil:
		r.logf("failed: %v; keeping %s", err, current)
		return st
	}
	var answer struct {
		Model *string `json:"model"`
	}
	if err := jsonv2.Unmarshal(bytes.TrimSpace(out), &answer); err != nil {
		r.logf("answer is not a JSON object with a string model: %v; keeping %s", err, current)
		return st
	}
	if answer.Model == nil {
		return st
	}
	chosen := strings.TrimSpace(*answer.Model)
	base := strings.TrimSuffix(current, "[1m]")
	if chosen == "" || chosen == current || chosen == base {
		return st
	}
	for _, m := range served {
		if m == chosen {
			st.model, st.notice = chosen, true
			r.logf("%s -> %s for this turn", current, chosen)
			return st
		}
	}
	r.logf("%s is not served by this profile; keeping %s", chosen, current)
	return st
}

// noticed records that thread's notice reached the client, so the rest of the
// turn carries it no more.
func (r *TurnRouter) noticed(thread string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threads[thread]; st != nil {
		st.notice = false
	}
}

// noticeText names a switch before the answer it applies to (promise 4).
func noticeText(from, to string) string {
	return "[ccmorph] Turn router: this turn is answered by " + to + " (you asked for " + from + ").\n\n"
}

// routeTurn asks the router about this request and returns the body to send,
// the notice the turn's first answer carries, and the thread it belongs to.
func (s *Server) routeTurn(body []byte, key, session string) (out []byte, notice, thread string) {
	t, ok := readTurn(body, session)
	if !ok {
		return body, "", ""
	}
	d := parseDoc(body)
	if d == nil {
		return body, "", ""
	}
	current, ok := d.str("model")
	if !ok {
		return body, "", ""
	}
	model, notice, thread := s.TurnRouter.route(t, session, current, contextTokens(d), func() []string {
		return s.Routes.servedModels(key, current)
	})
	if model == "" || model == current {
		return body, "", thread
	}
	nb := withModel(d, model)
	if s.Routes.command != nil && s.Routes.ensure(nb, key, nil, pathMessages) != "" {
		s.TurnRouter.logf("%s could not be prepared; keeping %s", model, current)
		return body, "", thread
	}
	return nb, notice, thread
}

// contextTokens is a rough size for the router to judge the turn by: one token
// per four bytes of the messages array.
func contextTokens(d *doc) int {
	sp, ok := d.last("messages")
	if !ok {
		return 0
	}
	return (sp.End - sp.Start) / 4
}
