package shim

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func noticeOut(t *testing.T, in, text string) string {
	t.Helper()
	n := &noticeStream{src: &thinkingStream{r: bufio.NewReaderSize(strings.NewReader(in), 64<<10)}, text: text, at: -1}
	var b strings.Builder
	for {
		line, err := n.readline()
		if err != nil {
			break
		}
		b.Write(line)
	}
	return b.String()
}

func start(index int, typ string) string {
	return `event:content_block_start` + "\n" + `data:{"type":"content_block_start","index":` +
		itoa(index) + `,"content_block":{"type":"` + typ + `","text":""}}` + "\n\n"
}

func delta(index int, text string) string {
	return `event:content_block_delta` + "\n" + `data:{"type":"content_block_delta","index":` +
		itoa(index) + `,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n"
}

func stop(index int) string {
	return `event:content_block_stop` + "\n" + `data:{"type":"content_block_stop","index":` +
		itoa(index) + `}` + "\n\n"
}

func itoa(n int) string { return strconv.Itoa(n) }

const notice = "[ccmorph] Turn router: this turn is answered by m2 (you asked for m1).\n\n"

func TestNoticeStreamPlacement(t *testing.T) {
	// A text reply: the notice takes index 0 and the answer shifts to 1.
	out := noticeOut(t, start(0, "text")+delta(0, "hi")+stop(0)+
		`event:message_stop`+"\n"+`data:{"type":"message_stop"}`+"\n\n", notice)
	if !strings.Contains(out, `"index":0,"delta":{"type":"text_delta","text":"[ccmorph] Turn router`) {
		t.Fatalf("notice not at 0:\n%s", out)
	}
	if !strings.Contains(out, `"index":1,"content_block":{"type":"text","text":""}`) ||
		!strings.Contains(out, `"index":1,"delta":{"type":"text_delta","text":"hi"}`) {
		t.Fatalf("the answer did not shift to 1:\n%s", out)
	}

	// Thinking first: the notice follows it, because a reply that starts with
	// thinking must keep it there, and every later block shifts by one.
	out = noticeOut(t, start(0, "thinking")+
		`event:content_block_delta`+"\n"+`data:{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`+"\n\n"+
		stop(0)+start(1, "text")+delta(1, "hi")+stop(1), notice)
	if !strings.Contains(out, `"index":0,"content_block":{"type":"thinking"`) {
		t.Fatalf("thinking moved:\n%s", out)
	}
	if !strings.Contains(out, `"index":1,"delta":{"type":"text_delta","text":"[ccmorph]`) {
		t.Fatalf("notice not after thinking:\n%s", out)
	}
	if !strings.Contains(out, `"index":2,"delta":{"type":"text_delta","text":"hi"}`) {
		t.Fatalf("the answer did not shift:\n%s", out)
	}

	// Thinking only: the notice is the block before the closing delta.
	out = noticeOut(t, start(0, "thinking")+stop(0)+
		`event:message_delta`+"\n"+`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`+"\n\n", notice)
	if !strings.Contains(out, `"index":1,"content_block":{"type":"text","text":""}`) {
		t.Fatalf("notice not at 1:\n%s", out)
	}

	// An error before any content leaves the stream exactly as it came.
	errStream := `event:error` + "\n" + `data:{"type":"error","error":{"type":"overloaded_error"}}` + "\n\n"
	if got := noticeOut(t, errStream, notice); got != errStream {
		t.Fatalf("an error stream must pass through:\n%s", got)
	}
}

func TestInsertNoticeJSON(t *testing.T) {
	text := "[ccmorph] routed\n\n"
	decode := func(t *testing.T, body string) []any {
		t.Helper()
		var v struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		out := make([]any, len(v.Content))
		for i, c := range v.Content {
			out[i] = c["text"]
		}
		return out
	}
	for _, c := range []struct {
		name, body string
		want       []any
		unchanged  bool
	}{
		{"a text answer", `{"model":"m","content":[{"type":"text","text":"hi"}]}`,
			[]any{text, "hi"}, false},
		{"after thinking", `{"content":[{"type":"thinking","thinking":"p"},{"type":"text","text":"hi"}]}`,
			[]any{"hi"}, false},
		{"thinking only", `{"content":[{"type":"thinking","thinking":"p"}]}`, nil, false},
		{"empty content", `{"content":[]}`, []any{text}, false},
		{"no content", `{"error":{"message":"x"}}`, nil, true},
	} {
		out, ok := insertNotice([]byte(c.body), text)
		if c.unchanged {
			if ok || string(out) != c.body {
				t.Fatalf("%s: %s", c.name, out)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s: not inserted", c.name)
		}
		got := decode(t, string(out))
		switch c.name {
		case "after thinking":
			// Thinking is not text; compare kinds instead.
			var v struct {
				Content []map[string]any `json:"content"`
			}
			json.Unmarshal(out, &v)
			if len(v.Content) != 3 || v.Content[1]["text"] != text || v.Content[2]["text"] != "hi" {
				t.Fatalf("%s: %s", c.name, out)
			}
			continue
		case "thinking only":
			var v struct {
				Content []map[string]any `json:"content"`
			}
			json.Unmarshal(out, &v)
			if len(v.Content) != 2 || v.Content[1]["text"] != text {
				t.Fatalf("%s: %s", c.name, out)
			}
			continue
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: %v", c.name, got)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%s: block %d = %v want %v", c.name, i, got[i], c.want[i])
			}
		}
	}
}
