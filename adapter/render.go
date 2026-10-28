package adapter

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/akleizor/ccmorph/compat"
	"github.com/akleizor/ccmorph/shim"
)

// q is a value as a YAML double-quoted scalar: Python's json.dumps escaping
// (ASCII only, so the output is byte-identical), which is a subset of YAML's.
// Every interpolated value goes through this: an unescaped quote or newline in a
// model id, a path or a credential would reshape the document.
func q(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case r > 0xFFFF:
			a, c := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, a, c)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// shimUserAgent is the passthrough User-Agent the shim states for this profile,
// or "". The shim the adapter will run (the stock one, or a profile's own build)
// prints it for `translator-user-agent`, from the profile's CCMORPH_* env.
// Failing soft is deliberate: the shim dies loudly at spawn when it is broken.
func shimUserAgent(shimPath string, shimEnv []string) string {
	if shimPath == "" { // the stock shim runs in this process: ask its hooks
		h, err := compat.FromGetenv(func(k string) string {
			for i := len(shimEnv) - 1; i >= 0; i-- {
				if v, ok := strings.CutPrefix(shimEnv[i], k+"="); ok {
					return v
				}
			}
			return ""
		})
		if err != nil || h.TranslatorUserAgent == nil {
			return ""
		}
		return strings.TrimSpace(h.TranslatorUserAgent())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shimPath, "translator-user-agent")
	cmd.Env = shimEnv
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func block(names []string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "      - name: %s\n        alias: %s\n", q(n), q(n))
	}
	return b.String()
}

func render(rows []Row, key string, port int, upstream, authdir, proxy, shimPath string, shimEnv []string, cloakNever bool) string {
	// Belt to inventory's braces: a row cached before ids were validated could
	// still carry YAML metacharacters, and one such id breaks the whole config.
	var safe []Row
	for _, r := range rows {
		if safeModelID.MatchString(r.Model) {
			safe = append(safe, r)
		} else {
			logf("omitting model id with unsafe characters: %s", shim.PyRepr(truncRunes(r.Model, 60)))
		}
	}
	rows = safe
	ids := func(dialects ...string) []string {
		var out []string
		for _, r := range rows {
			for _, d := range dialects {
				if r.Dialect == d {
					out = append(out, r.Model)
					break
				}
			}
		}
		return out
	}
	passthrough, responses := ids("anthropic"), ids("responses")
	ua := ""
	if len(passthrough) > 0 {
		ua = shimUserAgent(shimPath, shimEnv)
	}
	cloakLine := ""
	if cloakNever {
		cloakLine = "    cloak:\n      mode: \"never\"\n"
	}
	claudeHdr := ""
	if ua != "" {
		claudeHdr = "    headers:\n      User-Agent: " + q(ua) + "\n"
	}
	chat, noreason := ids("chat", "chat-noreason"), ids("chat-noreason")
	openaiBase := apiURL(upstream, "/v1")
	caps := map[string]int{}
	for _, r := range rows {
		if r.capped() {
			caps[r.Model] = *r.Cap
		}
	}
	// The thinking override is an Anthropic-dialect field: a row of any other
	// dialect carrying nothink must not produce one, or cliproxyapi would
	// translate it into a parameter the upstream rejects.
	nothink := map[string]bool{}
	for _, r := range rows {
		if r.Nothink && r.Dialect == "anthropic" {
			nothink[r.Model] = true
		}
	}

	// env.CCMORPH_UPSTREAM_PROXY sends the translator's upstream calls through an
	// HTTP CONNECT proxy. macOS denies a LAN gateway to a translator started under
	// a launchd-owned tmux server ("connect: no route to host"); a loopback proxy
	// holding a Local Network grant dials it instead.
	proxyLine := ""
	if proxy != "" {
		proxyLine = "proxy-url: " + q(proxy) + "\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, `host: "127.0.0.1"
port: %d
auth-dir: %s
%s
# Re-send a request the upstream dropped transiently (unexpected EOF, 408, or
# 5xx) before Claude Code ever sees the error. Only pre-stream failures can be
# retried -- once the SSE stream has emitted bytes a mid-stream drop is already
# downstream -- so this trims visible error noise, not every failure. 403 and 429
# are in cliproxyapi's retry set too (8.0.45), resent at once without backoff.
request-retry: 3

# The Responses provider injects an image_generation tool by default, which some
# upstreams reject ("imagegen deployment must be provided through header
# x-ms-oai-image-generation-deployment"). "chat" stops the injection.
disable-image-generation: "chat"

# Inbound key is the profile's own key: this instance serves exactly one profile,
# so nothing new is minted and no second credential exists.
api-keys:
  - %s
`, port, q(authdir), proxyLine, q(key))

	if len(passthrough) > 0 {
		fmt.Fprintf(&out, `
# Passthrough for models that already speak Anthropic.
#
# The models list is an ALLOWLIST for this entry, so it must be complete: any id
# omitted becomes "unknown provider". Never add a second entry of the same type to
# hold extras either -- entries are pooled and round-robined, which makes model
# resolution depend on which entry a request happens to land on.
#
# disable-cooling: a single credential has nothing to fail over to, so cooling
# would take the route offline as "auth_unavailable" after one transient error.
# rebuild-mid-system-message: Claude Code sends mid-conversation system content as
# a messages entry with role "system", which vLLM-backed upstreams reject with a
# literal_error on the role.
# Cloaking: before 8.0.34 a client whose User-Agent is not claude-cli is cloaked by
# default (billing header rewritten to a frozen cc_version, extra system reminder,
# fake session ids), so "never" is written there. From 8.0.34 "never" instead
# strips Claude Code's billing header, identity block and metadata.user_id and
# rewrites the User-Agent, while no block forwards the caller verbatim, so it is
# omitted.
claude-api-key:
  - api-key: %s
    base-url: %s
    disable-cooling: true
    rebuild-mid-system-message: true
%s%s    models:
%s`, q(key), q(upstream), cloakLine, claudeHdr, block(passthrough))
	}

	if len(responses) > 0 {
		fmt.Fprintf(&out, `
# Reasoning models need the Responses dialect: gateways reject function tools
# combined with reasoning_effort on chat/completions and say so explicitly.
codex-api-key:
  - api-key: %s
    base-url: %s
    disable-cooling: true
    models:
%s`, q(key), q(openaiBase), block(responses))
	}

	if len(chat) > 0 {
		fmt.Fprintf(&out, `
# Non-reasoning models, which /responses rejects because the translation carries
# encrypted reasoning content they do not accept.
openai-compatibility:
  - name: "chat"
    base-url: %s
    disable-cooling: true
    api-key-entries:
      - api-key: %s
    models:
%s`, q(openaiBase), q(key), block(chat))
	}

	if len(noreason) > 0 || len(caps) > 0 || len(nothink) > 0 || len(passthrough) > 0 {
		out.WriteString("\npayload:\n")
	}
	if len(noreason) > 0 || len(passthrough) > 0 {
		out.WriteString("  filter:\n")
	}
	if len(passthrough) > 0 {
		out.WriteString(`  # cliproxyapi adds thinking.block_binding when a Claude 5.5 history replays signed
  # thinking; Bedrock-backed gateways 400 it ("Extra inputs are not permitted").
  # Filter rules run after that injection, so the field never leaves.
    - models:
        - name: "*"
          protocol: "claude"
      params:
        - "thinking.block_binding"
`)
	}
	if len(noreason) > 0 {
		var names strings.Builder
		for _, n := range noreason {
			fmt.Fprintf(&names, "        - name: %s\n", q(n))
		}
		fmt.Fprintf(&out, `  # Claude Code always sends thinking for ids outside its catalogue, which becomes
  # reasoning_effort. These models reject that parameter but work without it, so it
  # is stripped rather than the models being dropped.
    - models:
%s      params:
        - "reasoning_effort"
`, names.String())
	}
	if len(caps) > 0 || len(nothink) > 0 {
		set := map[string]bool{}
		for n := range caps {
			set[n] = true
		}
		for n := range nothink {
			set[n] = true
		}
		sorted := make([]string, 0, len(set))
		for n := range set {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		var entries strings.Builder
		for _, name := range sorted {
			params := ""
			if c, ok := caps[name]; ok {
				params += `        "max_tokens": ` + strconv.Itoa(c) + "\n"
			}
			if nothink[name] {
				// The gateway reasons by default for these models and ignores
				// budget_tokens, so thinking can only be turned off. Nothing else
				// belongs in this override: cliproxyapi REPLACES the system field,
				// which would throw away Claude Code's entire system prompt.
				params += `        "thinking": {"type": "disabled"}` + "\n"
			}
			fmt.Fprintf(&entries, "    - models:\n        - name: %s\n      params:\n%s", q(name), params)
		}
		fmt.Fprintf(&out, `  # Claude Code sends max_tokens=%d; some models cap lower and reject the
  # request outright, so the cap each model states in its own error is clamped in.
  # Models probed to reason even when thinking was never requested get thinking
  # disabled outright: unbounded thinking-only turns are where the give-up loops
  # and empty turns gestate, and the gateway ignores budget_tokens so there is no
  # way to bound them. Never add "system" here -- the translator replaces the
  # system field, which would delete Claude Code's own system prompt.
  override:
%s`, ccMaxTokens, entries.String())
	}
	return out.String()
}

// cloakNeverFor reports whether the translator at path is older than 8.0.34 and
// so needs `cloak: never`. -h prints the version banner and exits 0 on 7.2.x and
// 8.0.x alike. A missing or unparseable version counts as new: omitting the block
// is the safe shape there.
func cloakNeverFor(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, "-h").CombinedOutput()
	return cloakNeverForBanner(string(out))
}

var bannerVersion = regexp.MustCompile(`CLIProxyAPI Version: v?(\d+)\.(\d+)\.(\d+)`)

func cloakNeverForBanner(out string) bool {
	m := bannerVersion.FindStringSubmatch(out)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return maj < 8 || (maj == 8 && (min == 0 && pat < 34))
}
