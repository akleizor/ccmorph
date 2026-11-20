package adapter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"net/http"
	"strings"
	"testing"
)

// #646: the shim keeps the translator's reasoning carrier only on the models
// the translator serves on its chat path.
func TestTranslatorChatModelsNamesOnlyTheChatRows(t *testing.T) {
	rows := []Row{
		{Model: "claude-opus-4-5", Dialect: "anthropic"},
		{Model: "gemini-4-carbon", Dialect: "chat"},
		{Model: "gpt-5", Dialect: "responses"},
		{Model: "glm-5", Dialect: "chat-noreason"},
		{Model: "dead", Dialect: "unusable"},
	}
	if got := strings.Join(translatorChatModels(rows), ","); got != "gemini-4-carbon,glm-5" {
		t.Fatalf("got %q", got)
	}
	if got := translatorChatModels([]Row{row("anthropic")}); len(got) != 0 {
		t.Fatalf("an all-Anthropic profile names %q", got)
	}
}

// The direct route has no translator, so its shim never gets the list and
// strips the carrier with every other thinking block.
func TestTheDirectRouteNamesNoTranslatorChatModels(t *testing.T) {
	cmd := spawnedDirect(t, map[string]string{"CCMORPH_SHIM_HOOKS": "/stable/hooks.py"})
	if env := strings.Join(cmd.Env, "\n"); strings.Contains(env, "CCMORPH_TRANSLATOR_CHAT_MODELS") {
		t.Fatalf("the direct route's shim env names translator chat models")
	}
}

// Behind the translator the shim's env names the chat-path models, so its 400
// resend keeps their carrier; the Anthropic passthrough row is not named.
func TestTheTranslatorRouteHandsItsChatModelsToTheShim(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	stub := filepath.Join(dir, "cliproxyapi")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	swap(t, &translatorBinary, func() string { return stub })
	swap(t, &servingFn, func(int, string) bool { return true })
	ports := 45990
	swap(t, &freePortFn, func() (int, error) { ports++; return ports, nil })
	var got *exec.Cmd
	var chat []byte
	swap(t, &spawnShim, func(c *exec.Cmd) (*proc, error) {
		got = c
		for _, kv := range c.Env {
			if v, ok := strings.CutPrefix(kv, "CCMORPH_TRANSLATOR_CHAT_MODELS_FILE="); ok {
				chat, _ = os.ReadFile(v) // before exit removes the adapter's state dir
			}
		}
		return nil, fmt.Errorf("abandoned")
	})
	rows := []Row{
		{Model: "claude-opus-4-5", Dialect: "anthropic", Status: 200},
		{Model: "gemini-4-carbon", Dialect: "chat", Status: 200},
	}
	captureStdio(t)
	exits(t, func() {
		serve(&cliArgs{profile: "demo", reserved: -1}, rows, "", "https://gateway", "k", "id", "",
			"/stable/hooks.py", map[string]string{"CCMORPH_SHIM_HOOKS": "/stable/hooks.py"}, map[string]string{}, nil)
	})
	if got == nil {
		t.Fatal("the shim was never spawned")
	}
	if chat == nil {
		t.Fatal("the shim env names no translator chat model list")
	}
	if string(chat) != "gemini-4-carbon\n" {
		t.Fatalf("chat list %q", chat)
	}
}

// A model selected mid-session is prepared by another adapter process; the list
// the shim rereads must name it afterwards (#646).
func TestPreparingAModelMidSessionRewritesTheChatList(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(conf, []byte("port: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatModelsFile(conf), []byte("gpt-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &inventoryFn, func(string, string, *http.Client) ([]any, []string, error) {
		return nil, []string{"gemini-4-carbon"}, nil
	})
	swap(t, &classifyFn, func(_, _, _, req string, _ bool) ([]Row, error) {
		return []Row{{Model: "claude-opus-4-5", Dialect: "anthropic", Status: 200},
			{Model: req, Dialect: "chat", Status: 200}}, nil
	})
	captureStdio(t)
	exits(t, func() {
		ensureConfig(&cliArgs{ensureConfig: conf, ensurePort: 45999, ensureIdentity: "id"},
			"gemini-4-carbon", "https://gw", "k", "id", "", "", "", nil)
	})
	b, err := os.ReadFile(chatModelsFile(conf))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "gemini-4-carbon\n" {
		t.Fatalf("chat list %q", b)
	}
}
