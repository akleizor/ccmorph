package compat

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tools(names ...string) string {
	var parts []string
	for _, n := range names {
		parts = append(parts, `{"name":"`+n+`","input_schema":{"properties":{"name":{}}}}`)
	}
	return `"tools":[` + strings.Join(parts, ",") + `]`
}

func result(inner string) string {
	return `"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[` + inner + `]}]}]`
}

func TestRepairToolReferences(t *testing.T) {
	declared := `{"type":"tool_reference","tool_name":"Read"}`
	for _, tc := range []struct {
		name, in, want string // want "" means untouched
	}{
		{"dangling becomes text",
			`{` + tools("Read") + `,` + result(`{"type":"tool_reference","tool_name":"Gone"}`) + `}`,
			`{` + tools("Read") + `,` + result(`{"type":"text","text":"[tool_reference: Gone]"}`) + `}`},
		{"no name",
			`{` + tools("Read") + `,` + result(`{"type":"tool_reference"}`) + `}`,
			`{` + tools("Read") + `,` + result(`{"type":"text","text":"[tool_reference]"}`) + `}`},
		{"declared is untouched", `{` + tools("Read") + `,` + result(declared) + `}`, ""},
		{"grouped toolset declares",
			`{"tools":[{"name":"set","tools":[{"name":"Read"}]}],` + result(declared) + `}`, ""},
		{"depth three names do not declare",
			`{"tools":[{"name":"set","tools":[{"name":"x","tools":[{"name":"Read"}]}]}],` + result(declared) + `}`,
			`{"tools":[{"name":"set","tools":[{"name":"x","tools":[{"name":"Read"}]}]}],` + result(`{"type":"text","text":"[tool_reference: Read]"}`) + `}`},
		{"schema property named name does not declare",
			`{"tools":[{"name":"A","input_schema":{"name":"Read"}}],` + result(declared) + `}`,
			`{"tools":[{"name":"A","input_schema":{"name":"Read"}}],` + result(`{"type":"text","text":"[tool_reference: Read]"}`) + `}`},
		{"no tools at all makes every reference dangling",
			`{` + result(declared) + `}`, `{` + result(`{"type":"text","text":"[tool_reference: Read]"}`) + `}`},
		{"reference outside a tool_result is left alone",
			`{"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"Gone"}]}]}`, ""},
		{"string content", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`, ""},
		{"not an object", `[1]`, ""},
		{"spacing and key order survive",
			"{ \"z\":1 ,\"tools\":[],\n\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"content\":[ {\"type\":\"tool_reference\",\"tool_name\":\"G\"} ]}]}] }",
			"{ \"z\":1 ,\"tools\":[],\n\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"content\":[ {\"type\":\"text\",\"text\":\"[tool_reference: G]\"} ]}]}] }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repairToolReferences([]byte(tc.in))
			if tc.want == "" {
				if ok || got != nil {
					t.Fatalf("changed: %s", got)
				}
				return
			}
			if !ok || string(got) != tc.want {
				t.Fatalf("got %s", got)
			}
		})
	}
}

func TestResendWithoutThinking(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"strips both kinds, keeps the rest",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":"s"},{"type":"redacted_thinking","data":"d"},{"type":"text","text":"a"}]}]}`,
			`{"messages":[{"role":"assistant","content":[{"type":"text","text":"a"}]}]}`},
		{"an emptied turn gets a placeholder",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking"}]}]}`,
			`{"messages":[{"role":"assistant","content":[{"type":"text","text":"."}]}]}`},
		{"user turns are not touched",
			`{"messages":[{"role":"user","content":[{"type":"thinking"}]}]}`, ""},
		{"no thinking", `{"messages":[{"role":"assistant","content":"x"}]}`, ""},
		{"top-level thinking config is not a block", `{"thinking":{"type":"enabled"},"messages":[]}`, ""},
		{"not an object", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resendWithoutThinking([]byte(tc.in), nil)
			if string(got) != tc.want {
				t.Fatalf("got %s", got)
			}
		})
	}
}

func TestAffinityHeader(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		ok           bool
	}{
		{"plain", "X-Session-Affinity", true},
		{"empty means off", "", true},
		{"space", "X Bad", false},
		{"colon", "X:Bad", false},
		{"underscore", "X_Bad", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := build(tc.header, "")
			if (err == nil) != tc.ok {
				t.Fatal(err)
			}
			if !tc.ok || tc.header == "" {
				if tc.ok && h.OutboundHeaders != nil {
					t.Fatal("hook set while off")
				}
				return
			}
			hd := http.Header{}
			hd["x-session-affinity"] = []string{"stale"} // non-canonical variant
			hd.Add("X-Session-Affinity", "old")
			h.OutboundHeaders(hd, "sess-1")
			if len(hd) != 1 || hd.Get("X-Session-Affinity") != "sess-1" {
				t.Fatal(hd)
			}
			hd = http.Header{}
			h.OutboundHeaders(hd, "")
			if len(hd) != 0 {
				t.Fatal("no session must add no header", hd)
			}
		})
	}
}

func TestUserAgent(t *testing.T) {
	h, err := build("", "claude-cli/{version} (external, cli)")
	if err != nil {
		t.Fatal(err)
	}
	for client, want := range map[string]string{
		"claude-cli/2.1.300 (external, sdk-cli)": "claude-cli/2.1.300 (external, cli)",
		"claude-cli/2.1.300":                     "claude-cli/2.1.300 (external, cli)",
		"curl/8":                                 "curl/8",
		"":                                       "",
	} {
		if got := h.UserAgent(client); got != want {
			t.Errorf("%q -> %q, want %q", client, got, want)
		}
	}
	off, _ := build("", "")
	if off.UserAgent != nil || off.TranslatorUserAgent != nil {
		t.Fatal("user agent hooks set while off")
	}
}

func TestTranslatorUserAgent(t *testing.T) {
	h, _ := build("", "claude-cli/{version} (external, cli)")
	fake := func(script string) {
		dir := t.TempDir()
		if script != "" {
			os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+script+"\n"), 0o755)
		}
		t.Setenv("PATH", dir)
	}
	fake(`echo "2.1.301 (Claude Code)"`)
	if got := h.TranslatorUserAgent(); got != "claude-cli/2.1.301 (external, cli)" {
		t.Fatal(got)
	}
	fake(`echo "not a version"`)
	if got := h.TranslatorUserAgent(); got != "" {
		t.Fatal(got)
	}
	fake("") // claude not installed
	if got := h.TranslatorUserAgent(); got != "" {
		t.Fatal(got)
	}
}

func TestFromEnvReadsTheProfileEnv(t *testing.T) {
	t.Setenv("CCMORPH_AFFINITY_HEADER", "bad name")
	if _, err := FromEnv(); err == nil {
		t.Fatal("invalid header name must be a startup error")
	}
	t.Setenv("CCMORPH_AFFINITY_HEADER", "X-A")
	t.Setenv("CCMORPH_USER_AGENT", "ua/{version}")
	h, err := FromEnv()
	if err != nil || h.OutboundHeaders == nil || h.UserAgent == nil || h.RepairRequest == nil || h.ResendBody == nil {
		t.Fatal(h, err)
	}
}
