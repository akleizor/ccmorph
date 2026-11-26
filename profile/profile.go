// Package profile reads and validates one <name>.conf: the one reader the zsh
// library and the core share. Read returns the fields as the launcher sees
// them: keychain references resolved and the legacy `proxy` type folded.
package profile

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/akleizor/ccmorph/ordjson"
)

// Lists are the env-key allowlists a conf is checked against; Defaults() builds
// them from ProfileEnv and MappingEnv in envkeys.go.
type Lists struct {
	ProfileEnv, MappingEnv map[string]bool
}

// Error is a refusal the caller prints as-is. Repair, when set, is the one line
// that makes the conf load again: the skip message a listing appends to the
// profile it could not read (GH-839).
type Error struct {
	Msg    string
	Repair string
}

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// failRepair is fail for a refusal whose fault has a known edit: repair is the
// line a listing appends to the profile it could not read (GH-839).
func failRepair(repair, format string, a ...any) error {
	return &Error{Msg: fmt.Sprintf(format, a...), Repair: repair}
}

// Repair is the one line that fixes a conf Read refused: the edit the refusal
// names when it names one, then the removal that always works (GH-839).
func Repair(err error, name string) string {
	if r := RepairOf(err); r != "" {
		return r
	}
	return "fix " + name + ".conf, or delete it: " + rmHint(name)
}

// rmHint is the ccmorph removal a repair prints for a name. A flag-shaped name
// is put after `--`, so the line names the profile rather than opening help or
// turning into a flag (GH-915); the name itself is shell-quoted when a shell
// would rewrite it.
func rmHint(name string) string {
	if strings.HasPrefix(name, "-") {
		return "ccmorph rm -- " + ShellWord(name)
	}
	return "ccmorph rm " + ShellWord(name)
}

// shellSafeWord is a word a shell runs literally: no glob, quote or separator
// rune needs quoting for it.
var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellWord is a word as a printed command should carry it (GH-915): a word a
// shell runs literally stays bare — every name the validator accepts is one —
// and anything else is single-quoted so the line a listing prints can be
// pasted back into bash or zsh unchanged. A leading `=` is quoted even though
// the rune is safe elsewhere: zsh expands an unquoted `=word` to the command's
// path, so `ccmorph rm =ls` would reach ccmorph as `/usr/bin/ls`.
func ShellWord(s string) string {
	if s != "" && s[0] != '=' && shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// OwnStateName reports whether name can spell private state of its own: a
// single path element that is not the state directory itself. The scanner
// lists a conf under any stem the directory holds — "", "." and ".." from
// .conf, ..conf and ...conf — but those three collapse onto the profiles
// directory, the state root, or nothing at all. rm and mv act on such a
// name's conf only, never on a directory it would share with every profile
// (GH-915).
func OwnStateName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/')
}

// RepairOf is the repair a refusal carries, or "" when it names none (GH-839).
func RepairOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Repair
	}
	return ""
}

// Removed keys still launch: each says once what replaced it and is dropped.
var removed = map[string]string{
	"transport":      "transport= was removed; export HTTPS_PROXY from your bridge instead",
	"forward_ip":     "forward_ip= was removed; the adapter no longer sends X-Forwarded-For",
	"subagent_model": "subagent_model= was removed; it was never read",
	"lean":           "lean= was removed; launch behavior no longer varies by it",
	"role":           "role= was removed; pick the model with --model instead",
}

// Efforts is the one list of effort levels, lowest to highest: the conf
// validator, the store's set, the pickers and the help all name it, so a level
// here is a level everywhere (#264).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// EffortList names the accepted levels as prose — "low, medium, high, xhigh or
// max" — for every refusal that names them.
func EffortList() string {
	ls := Efforts
	return strings.Join(ls[:len(ls)-1], ", ") + " or " + ls[len(ls)-1]
}

// effortMsg is the one sentence for an effort level the conf and the store do
// not accept: the value, the profile and every level that is accepted, so the
// reader knows what to type instead.
func effortMsg(name, value string) string {
	return fmt.Sprintf("invalid effort '%s' in '%s': effort must be %s", value, name, EffortList())
}

// EffortError is effortMsg as the store's error: a saved default the store
// refuses names the same value and levels as the conf reader (GH-839).
func EffortError(name, value string) error { return fail("%s", effortMsg(name, value)) }

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	budgetRE = regexp.MustCompile(`^([0-9]+\.?[0-9]*|\.[0-9]+)$`)
	digits   = regexp.MustCompile(`^[0-9]+$`)
	modelID  = regexp.MustCompile(`^[A-Za-z0-9._:\[\]()@/-]+$`)
	common   = []string{"type", "model", "model_defaults", "private", "monthly_budget_usd", "effort", "effort.*", "context_discovered"}
	perType  = map[string][]string{
		"sub":      {"email", "usage_route"},
		"token":    {"token", "usage_route"},
		"apikey":   {"api_key", "base_url", "excluded_models", "adapter", "tag", "env.*", "context_window.*", "window_sources", "model_map"},
		"provider": {"provider", "excluded_models", "tag", "env.*", "context_window.*", "window_sources", "usage_route"},
	}
)

// MaxNameLen is the longest profile name. The longest file a profile makes is
// ".<name>.bridge.users" (+14), so 200 leaves every derived name far under
// NAME_MAX while staying past any name a person types.
const MaxNameLen = 200

// LegacyMaxNameLen is the longest name the rule before #373 could have stored:
// that rule had no length bound, and every profile makes "<name>.conf" and
// ".locks/<name>.lock" (+5), so 250 bytes is the last one inside NAME_MAX's
// 255. The bridge files (".<name>.bridge.users", +14) reach only 241, but they
// bound just the files a bridge-bearing provider makes: a name between the two
// has no bridge file to remove. RemovableName accepts this range so rm can
// remove a profile made before the limit (#449); every other verb still
// requires ValidName.
const LegacyMaxNameLen = 250

// nameShape is the part of a name that never depended on its length.
func nameShape(s string) bool {
	l := strings.ToLower(s)
	return nameRE.MatchString(s) && l != "active" && l != "ccmorph"
}

// ValidProfileName is the one gate for a profile name: [A-Za-z0-9_]+, at most
// MaxNameLen bytes, never a reserved name in any case. Both names the packages
// publish — ValidName here and ctxwindow.ValidProfileName — call it (#638), so
// the shape the launcher writes and the shape the core reads cannot drift.
func ValidProfileName(s string) bool {
	return len(s) <= MaxNameLen && nameShape(s)
}

// ValidName is the name profile's own callers use for the gate.
func ValidName(s string) bool { return ValidProfileName(s) }

// RemovableName reports whether name is one rm can still address: a name
// ValidName accepts, or one the pre-#373 rule accepted whose conf and lock
// still fit NAME_MAX. Only removal takes the wider range (#449).
func RemovableName(s string) bool {
	return len(s) <= LegacyMaxNameLen && nameShape(s)
}

// BadNameMsg is the one refusal every command gives a word that can never be a
// profile name (#373): one line, the reason, and the name shown short and
// printable.
func BadNameMsg(name string) string { return badNameMsg(name, MaxNameLen) }

// RemovableBadNameMsg is BadNameMsg in rm's voice: rm accepts the legacy range
// (#449), so the length reason names the bound rm applies, not the MaxNameLen
// every other verb enforces (#551).
func RemovableBadNameMsg(name string) string { return badNameMsg(name, LegacyMaxNameLen) }

// badNameMsg builds the refusal for a word under one length bound.
func badNameMsg(name string, max int) string {
	head := "invalid profile name '" + shownName(name) + "': "
	switch {
	case !nameRE.MatchString(name):
		return head + "a name may hold only A-Z, a-z, 0-9 and _ (no hyphen, dot, slash or space)"
	case len(name) > max:
		return head + "too long; at most " + strconv.Itoa(max) + " of A-Z, a-z, 0-9 and _"
	}
	return head + "that word is reserved; choose another"
}

// shownName is the name as a refusal shows it: at most 32 runes, with control
// and other non-printing runes replaced so a reply is always one short line.
func shownName(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		if i == 32 {
			b.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Read loads state/<name>.conf. warn receives one line per removed key. With
// secure, the file is chmod 600 first, as every writing caller wants.
func Read(state, name string, secure bool, lists Lists, warn func(string)) (map[string]string, error) {
	return readConf(state, name, secure, lists, warn, nil)
}

// ReadForSave is Read with one save's changes applied to the fields before they
// are checked: the conf that save will leave behind. A save that replaces a
// value the file refuses is its own repair, so it must be judged on what it
// writes, never on the value it overwrites (GH-839).
func ReadForSave(state, name string, secure bool, lists Lists, warn func(string), changes map[string]string) (map[string]string, error) {
	return readConf(state, name, secure, lists, warn, changes)
}

func readConf(state, name string, secure bool, lists Lists, warn func(string), changes map[string]string) (map[string]string, error) {
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
		checkV := v
		if ov, ok := changes[k]; ok {
			checkV = ov
		}
		if err := checkKey(name, k, checkV, lists); err != nil {
			return nil, err
		}
		fields[k] = v
	}
	for _, k := range []string{"api_key", "token"} {
		if ref := fields[k]; strings.HasPrefix(ref, "keychain:") {
			secret, err := KeychainGet(ref)
			if err != nil {
				return nil, failRepair("set it again with: ccmorph config "+name,
					"'%s': %s is not in the login keychain; set it again with: ccmorph config %s", name, ref, name)
			}
			fields[k] = secret
		}
	}
	return fields, checkType(name, fields, lists)
}

func checkKey(name, k, v string, lists Lists) error {
	switch {
	case k == "type" || k == "email" || k == "token" || k == "base_url" || k == "api_key" || k == "model" ||
		k == "adapter" || k == "tag" || k == "provider" || k == "context_discovered" || k == "window_sources" ||
		k == "usage_route":
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
		// A per-model level lives in the settings layer; a conf may carry one
		// too (the seed writes "effort.<model>"), and the empty "effort." names
		// no model at all.
		if slices.Contains(Efforts, v) && k != "effort." {
			break
		}
		if k == "effort" {
			return failRepair("set a valid level with: ccmorph set "+name+" effort <level>, or delete it: ccmorph rm "+name,
				"%s", effortMsg(name, v))
		}
		if k == "effort." {
			return failRepair("remove the key 'effort.' from "+name+".conf, or delete it: ccmorph rm "+name,
				"invalid effort key 'effort.' in '%s': effort must be %s", name, EffortList())
		}
		return failRepair("remove the key '"+k+"' from "+name+".conf, or delete it: ccmorph rm "+name,
			"%s", effortMsg(name, v))
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
		return failRepair("remove the unknown key '"+k+"' from "+name+".conf, or delete it: ccmorph rm "+name,
			"unknown key '%s' in '%s'", k, name)
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
			// The same repair `add` gives for a taken name and `doctor` gives
			// for a signed-out account (GH-649): the sign-in completes the
			// conf, so nothing else has to be thrown away first.
			return failRepair("sign in again: ccmorph login "+name,
				"Claude account profile '%s' has no email — sign in again: ccmorph login %s", name, name)
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
		return failRepair("remove the key '"+k+"' from "+name+".conf, or delete it: ccmorph rm "+name,
			"key '%s' is invalid for %s profile '%s'", k, f["type"], name)
	}
	return nil
}

func allowedKey(k string, patterns []string) bool {
	for _, p := range patterns {
		// A pattern is a literal key or a literal prefix ending in "*".
		if prefix, wild := strings.CutSuffix(p, "*"); k == p || wild && strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// ValidModelMap: a JSON object of unique model ids to model ids, no key carrying [1m].
func ValidModelMap(s string) bool {
	if !strings.HasPrefix(strings.TrimLeftFunc(s, unicode.IsSpace), "{") {
		return false
	}
	v, err := ordjson.Parse([]byte(s))
	if err != nil {
		return false
	}
	obj, ok := v.(*ordjson.Obj)
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
	// usage_route is the one opt-out from the usage hook; any other value would
	// read as a route that does not exist.
	if v := f["usage_route"]; v != "" && v != "direct" {
		return fail("profile '%s' usage_route must be 'direct' or absent", name)
	}
	if f["type"] != "apikey" {
		return nil
	}
	label := "API key profile '" + name + "'"
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
			return fail("%s has adapter=true but no endpoint to front", label)
		}
	default:
		return fail("%s adapter must be 'true', 'false', or absent", label)
	}
	return nil
}

// ValidateBaseURL refuses a base_url without an http(s) scheme or with whitespace.
func ValidateBaseURL(label, u string) error {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fail("%s endpoint must use http:// or https://", label)
	}
	if hasSpace(u) {
		return fail("%s endpoint contains whitespace", label)
	}
	return nil
}

// hasSpace is zsh [[:space:]] in a UTF-8 locale.
func hasSpace(s string) bool { return strings.IndexFunc(s, unicode.IsSpace) >= 0 }

// ProviderName is a provider id as a person sees it: xAI keeps its casing, any
// other id capitalises its first rune, and an empty id stays empty. The two
// Kimi ids are the exception that spells a name: kimi.ai and kimi.com are the
// global and the China service, and every list that names one — the sign-in
// methods, the API-key provider list, help add, the README — calls it what the
// provider presets call it, so the region tells them apart, not the domain
// (#366).
func ProviderName(p string) string {
	if p == "xai" {
		return "xAI"
	}
	if p == "kimi-ai" {
		return "Kimi For Coding (global)"
	}
	if p == "kimi" {
		return "Kimi For Coding (China)"
	}
	if p == "" {
		return p
	}
	r := []rune(p)
	return string(unicode.ToUpper(r[0])) + strings.ToLower(string(r[1:]))
}

// KindLabel is the one display name every screen uses for a profile kind: the
// settings in the file (sub, token, apikey) are spellings for the file, not for
// people. A provider profile is named by its provider.
func KindLabel(typ, provider string) string {
	switch typ {
	case "sub":
		return "Claude account"
	case "token":
		return "Claude token"
	case "apikey", "proxy":
		return "API key"
	case "provider":
		if provider == "" {
			return "Provider"
		}
		return ProviderName(provider)
	}
	return typ
}

// KindPhrase is KindLabel with the article a sentence puts before it: "an API
// key", never "a API key" (GH-790). A label whose first letter is a vowel
// takes "an"; every other kind keeps "a". The sentences that name a profile's
// kind — cred's refusal, login's, the model list's — read the article from
// here, so it agrees with the label wherever a kind is named.
func KindPhrase(typ, provider string) string {
	label := KindLabel(typ, provider)
	if label == "" {
		return label
	}
	if strings.ContainsRune("AEIOUaeiou", rune(label[0])) {
		return "an " + label
	}
	return "a " + label
}

// Env is the launch environment a profile's fields produce, in the order `ccmorph
// _env` prints it: the profile's own CLAUDE_CONFIG_DIR, the profile's name for the
// session to see (#275), the credential its type carries, the model map, then every
// env.* override. A sub profile authenticates from its own config dir and a provider
// gets its endpoint from the bridge at launch, so neither carries a credential here.
func Env(name, profileDirs string, f map[string]string) ([]string, error) {
	out := []string{"CLAUDE_CONFIG_DIR=" + profileDirs + "/" + name, "CCMORPH_PROFILE=" + name}
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
