package agentproxy

import (
	"sync"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
)

// A live profile switch takes effect at a session's next turn boundary, never
// inside a tool loop. Every backend wants its own reasoning artifacts on the
// assistant turns of the CURRENT loop (the turns after the last human prompt)
// and accepts a history without them before it, so a loop that changes backend
// halfway carries the old backend's thinking blocks into one that refuses them.
// The edge therefore pins a loop to the route that served the request that
// started it, and lets the next new turn take the route then held.

// midLoop is whether a request continues a tool loop: its final message is a
// user message holding a tool_result block. Judged by structure only. A
// string or text-only user message starts a new turn, and that includes the
// human-less continuations Claude Code sends as text (stop-hook feedback, a
// restored "continue"): the assistant turn before one has already ended, so it
// sits in the history before the last user message, where reasoning may be
// absent. A body that cannot be read as that shape is a new turn.
func midLoop(p *ordjson.Obj) bool {
	msgs := list(p, "messages")
	if len(msgs) == 0 {
		return false
	}
	last, _ := msgs[len(msgs)-1].(*ordjson.Obj)
	if last == nil {
		return false
	}
	if role, _ := str(last, "role"); role != "user" {
		return false
	}
	for _, b := range list(last, "content") {
		if blk, ok := b.(*ordjson.Obj); ok {
			if t, _ := str(blk, "type"); t == "tool_result" {
				return true
			}
		}
	}
	return false
}

// loopPin is the route one conversation's current loop is on.
type loopPin struct {
	target, via string
	seen        time.Time
}

// Pins live in memory only. One idle for pinTTL is dropped (a request after
// that takes the route then held, as after an edge restart), and pinMax bounds
// the table. Variables so a test can lower them.
var (
	pinTTL = 2 * time.Hour
	pinMax = 4096
)

type loopPins struct {
	mu sync.Mutex
	m  map[string]loopPin
}

// pinKey names one conversation: a subagent shares its session's header, and
// its own loop runs beside the main one, so the agent id is part of the key.
func pinKey(session, agent string) string { return session + "\x00" + agent }

func (l *loopPins) get(key string, now time.Time) (loopPin, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.m[key]
	if ok && now.Sub(p.seen) > pinTTL {
		delete(l.m, key)
		return loopPin{}, false
	}
	return p, ok
}

func (l *loopPins) set(key, target, via string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]loopPin{}
	}
	if _, ok := l.m[key]; !ok && len(l.m) >= pinMax {
		for k, p := range l.m {
			if now.Sub(p.seen) > pinTTL {
				delete(l.m, k)
			}
		}
		for len(l.m) >= pinMax {
			oldest, at := "", now
			for k, p := range l.m {
				if oldest == "" || p.seen.Before(at) {
					oldest, at = k, p.seen
				}
			}
			delete(l.m, oldest)
		}
	}
	l.m[key] = loopPin{target, via, now}
}
