// Package profile reads and validates one <name>.conf: the one reader the zsh
// library and the core share. Read returns the fields as the launcher sees
// them: keychain references resolved and the legacy `proxy` type folded.
package profile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/akleizor/ccmorph/pycompat"
)

// Lists are the env-key allowlists the zsh library owns (_CCMORPH_PROFILE_ENV,
// _CCMORPH_MODEL_MAPPING_ENV); they arrive from the caller so there is one copy.
type Lists struct {
	ProfileEnv, MappingEnv map[string]bool
}

// Error is a refusal the caller prints as-is.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{fmt.Sprintf(format, a...)} }

// Removed keys still launch: each says once what replaced it and is dropped.
var removed = map[string]string{
	"transport":      "transport= was removed; export HTTPS_PROXY from your bridge instead",
	"forward_ip":     "forward_ip= was removed; the adapter no longer sends X-Forwarded-For",
	"subagent_model": "subagent_model= was removed; it was never read",
	"lean":           "lean= was removed; launch behavior no longer varies by it",
	"role":           "role= was removed; pick the model with --model instead",
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	budgetRE = regexp.MustCompile(`^([0-9]+\.?[0-9]*|\.[0-9]+)$`)
	digits   = regexp.MustCompile(`^[0-9]+$`)
	modelID  = regexp.MustCompile(`^[A-Za-z0-9._:\[\]()@/-]+$`)
	efforts  = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	common   = []string{"type", "model", "model_defaults", "private", "monthly_budget_usd", "effort", "effort.*", "context_discovered"}
	perType  = map[string][]string{
		"sub":      {"email"},
		"token":    {"token"},
		"apikey":   {"api_key", "base_url", "excluded_models", "adapter", "tag", "env.*", "context_window.*", "window_sources", "model_map"},
		"provider": {"provider", "excluded_models", "tag", "env.*", "context_window.*", "window_sources"},
	}
)

// ValidName mirrors _ccm_valid: [A-Za-z0-9_]+, never a reserved name in any case.
func ValidName(s string) bool {
	l := strings.ToLower(s)
	return nameRE.MatchString(s) && l != "active" && l != "ccmorph"
}

// Read loads state/<name>.conf. warn receives one line per removed key. With
// secure, the file is chmod 600 first, as every writing caller wants.
func Read(state, name string, secure bool, lists Lists, warn func(string)) (map[string]string, error) {
	if !ValidName(name) {
		return nil, fail("invalid profile name")
	}
	path := filepath.Join(state, name+".conf")
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil, fail("invalid profile file for '%s'", name)
	}
	if secure {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fail("cannot secure profile '%s'", name)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fail("invalid profile file for '%s'", name)
	}
	fields := map[string]string{}
	text := strings.TrimSuffix(string(data), "\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if strings.Contains(line, "\r") || !ok {
			return nil, fail("malformed config for '%s'", name)
		}
		if k == "" || strings.ContainsAny(k, " \t\v\f") {
			return nil, fail("invalid key in '%s'", name)
		}
		if _, dup := fields[k]; dup {
			return nil, fail("duplicate key '%s' in '%s'", k, name)
		}
		if msg, ok := removed[k]; ok {
			warn("ccmorph: " + msg)
			continue
		}
		if err := checkKey(name, k, v, lists); err != nil {
			return nil, err
		}
		fields[k] = v
	}
	for _, k := range []string{"api_key", "token"} {
		if ref := fields[k]; strings.HasPrefix(ref, "keychain:") {
			secret, err := KeychainGet(ref)
			if err != nil {
				return nil, fail("'%s': %s is not in the login keychain; set it again with: ccmorph config %s", name, ref, name)
			}
			fields[k] = secret
		}
	}
	return fields, checkType(name, fields, lists)
}

func checkKey(name, k, v string, lists Lists) error {
	switch {
	case k == "type" || k == "email" || k == "token" || k == "base_url" || k == "api_key" || k == "model" ||
		k == "adapter" || k == "tag" || k == "provider" || k == "context_discovered" || k == "window_sources":
	case k == "excluded_models":
		if v == "" || strings.ContainsAny(v, " \t\v\f") || strings.HasPrefix(v, ",") || strings.HasSuffix(v, ",") || strings.Contains(v, ",,") {
			return fail("excluded_models must be comma-separated exact model IDs")
		}
	case k == "model_map":
		if !ValidModelMap(v) {
			return fail("model_map must be a JSON object of unique model IDs to model IDs, and no key may carry the [1m] suffix")
		}
	case k == "monthly_budget_usd":
		if !budgetRE.MatchString(v) || !strings.ContainsAny(v, "123456789") {
			return fail("monthly_budget_usd must be a positive number")
		}
	case k == "model_defaults":
		if v != "1" {
			return fail("invalid model defaults marker")
		}
	case k == "private":
		if v != "1" {
			return fail("private must be 1")
		}
	case k == "effort" || strings.HasPrefix(k, "effort."):
		if !efforts[v] || k == "effort." {
			return fail("invalid effort setting '%s'", k)
		}
	case strings.HasPrefix(k, "env."):
		if !lists.ProfileEnv[strings.TrimPrefix(k, "env.")] {
			return fail("disallowed environment key '%s' in '%s'", k, name)
		}
	case strings.HasPrefix(k, "context_window."):
		m := strings.TrimPrefix(k, "context_window.")
		if m == "" || strings.ContainsAny(m, "=\r\n") || !digits.MatchString(v) || strings.Trim(v, "0") == "" {
			return fail("invalid context pin '%s' in '%s'", k, name)
		}
	default:
		return fail("unknown key '%s' in '%s'", k, name)
	}
	return nil
}

func checkType(name string, f map[string]string, lists Lists) error {
	if f["type"] == "proxy" {
		// The old spelling of a key-authenticated routed endpoint, folded here once.
		if f["base_url"] == "" {
			return fail("proxy '%s' has no base_url", name)
		}
		f["type"] = "apikey"
	}
	switch f["type"] {
	case "sub":
		if f["email"] == "" {
			return fail("sub profile '%s' has no email — re-create it: ccmorph rm %s && ccmorph add %s", name, name, name)
		}
	case "token":
		if f["token"] == "" {
			return fail("token profile '%s' has no token", name)
		}
	case "apikey":
	case "provider":
		if f["provider"] == "" {
			return fail("provider profile '%s' has no provider", name)
		}
	default:
		return fail("invalid or missing type in '%s'", name)
	}
	allowed := append(append([]string{}, common...), perType[f["type"]]...)
	for k := range f {
		if allowedKey(k, allowed) || (strings.HasPrefix(k, "env.") && lists.MappingEnv[strings.TrimPrefix(k, "env.")]) {
			continue
		}
		return fail("key '%s' is invalid for %s profile '%s'", k, f["type"], name)
	}
	return nil
}

func allowedKey(k string, patterns []string) bool {
	for _, p := range patterns {
		if pycompat.Fnmatch(k, p) {
			return true
		}
	}
	return false
}

// ValidModelMap: a JSON object of unique model ids to model ids, no key carrying [1m].
func ValidModelMap(s string) bool {
	if !strings.HasPrefix(strings.TrimLeftFunc(s, pycompat.IsSpace), "{") {
		return false
	}
	v, err := pycompat.Parse([]byte(s))
	if err != nil {
		return false
	}
	obj, ok := v.(*pycompat.Obj)
	// A duplicate key collapses in the parsed object, so count the raw keys apart.
	if !ok || rawKeyCount(s) != obj.Len() {
		return false
	}
	for _, k := range obj.Keys() {
		val, _ := obj.Get(k)
		t, ok := val.(string)
		if !ok || !modelID.MatchString(k) || !modelID.MatchString(t) || strings.HasSuffix(strings.ToLower(k), "[1m]") {
			return false
		}
	}
	return true
}

// KeychainService is the login-keychain service every profile credential uses.
const KeychainService = "ccmorph"

// KeychainGet returns the secret behind a keychain:<account> reference.
func KeychainGet(ref string) (string, error) {
	b, err := exec.Command("security", "find-generic-password", "-s", KeychainService,
		"-a", strings.TrimPrefix(ref, "keychain:"), "-w").Output()
	return strings.TrimSuffix(string(b), "\n"), err
}

// rawKeyCount counts the members of the top-level object in already-validated JSON.
func rawKeyCount(s string) int {
	depth, n := 0, 0
	inStr, esc, sawValue := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			if depth == 1 && !sawValue {
				n++
				sawValue = true
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				sawValue = false
			}
		}
	}
	return n
}

// Validate checks a loaded profile beyond what Read enforces: an apikey profile
// must carry a clean key, and a base_url, when set, an http(s) scheme. sub, token
// and provider profiles need nothing further.
func Validate(name string, f map[string]string) error {
	if f["type"] != "apikey" {
		return nil
	}
	label := "apikey '" + name + "'"
	switch key := f["api_key"]; {
	case key == "":
		return fail("%s has no api_key", label)
	case hasSpace(key):
		return fail("%s api_key contains whitespace", label)
	}
	if f["base_url"] != "" {
		if err := ValidateBaseURL(label, f["base_url"]); err != nil {
			return err
		}
	}
	// adapter=true declares a foreign-dialect endpoint; a native key has nothing to translate.
	switch f["adapter"] {
	case "", "false":
	case "true":
		if f["base_url"] == "" {
			return fail("%s has adapter=true but no base_url to front", label)
		}
	default:
		return fail("%s adapter must be 'true', 'false', or absent", label)
	}
	return nil
}

// ValidateBaseURL refuses a base_url without an http(s) scheme or with whitespace.
func ValidateBaseURL(label, u string) error {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fail("%s base_url must use http:// or https://", label)
	}
	if hasSpace(u) {
		return fail("%s base_url contains whitespace", label)
	}
	return nil
}

// hasSpace is zsh [[:space:]] in a UTF-8 locale.
func hasSpace(s string) bool { return strings.IndexFunc(s, unicode.IsSpace) >= 0 }

// Env is the launch environment a profile's fields produce, in the order `ccmorph
// _env` prints it: the profile's own CLAUDE_CONFIG_DIR, the credential its type
// carries, the model map, then every env.* override. A sub profile authenticates
// from its own config dir and a provider gets its endpoint from the bridge at
// launch, so neither carries a credential here.
func Env(name, profileDirs string, f map[string]string) ([]string, error) {
	out := []string{"CLAUDE_CONFIG_DIR=" + profileDirs + "/" + name}
	if f["private"] == "1" {
		out = append(out, "CCMORPH_PRIVATE=1")
	}
	switch f["type"] {
	case "apikey":
		// No base_url is the native api.anthropic.com case: Claude Code's own default.
		if f["base_url"] != "" {
			out = append(out, "ANTHROPIC_BASE_URL="+f["base_url"])
		}
		out = append(out, "ANTHROPIC_API_KEY="+f["api_key"])
	case "token":
		out = append(out, "CLAUDE_CODE_OAUTH_TOKEN="+f["token"])
	case "provider", "sub":
	default:
		return nil, fail("profile '%s' has unsupported type '%s'", name, f["type"])
	}
	if f["model_map"] != "" {
		out = append(out, "CCMORPH_MODEL_MAP="+f["model_map"])
	}
	var keys []string
	for k := range f {
		if strings.HasPrefix(k, "env.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, "env.")+"="+f[k])
	}
	return out, nil
}
