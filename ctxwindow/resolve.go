package ctxwindow

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Limit is a context window derived from a models.dev `limit` object.
type Limit struct {
	Value          int64
	Input, Context *int64
}

func posIntPtr(v any) *int64 {
	if n, ok := posInt(v); ok {
		return &n
	}
	return nil
}

// LimitWindow derives {value, input, context} from a models.dev `limit` object.
// The operational budget is the tighter of input and context: an input
// sub-limit below the combined context still refuses a larger send.
func LimitWindow(limit any) *Limit {
	o, ok := limit.(map[string]any)
	if !ok {
		return nil
	}
	cv := o["context"]
	iv := o["input"]
	context, input := posIntPtr(cv), posIntPtr(iv)
	var value int64
	switch {
	case input != nil && context != nil:
		value = *input
		if *context < value {
			value = *context
		}
	case input != nil:
		value = *input
	case context != nil:
		value = *context
	default:
		return nil
	}
	return &Limit{value, input, context}
}

func objOf(v any) obj { o, _ := v.(map[string]any); return o }

func windowOf(entry obj) *Limit {
	v := entry["limit"]
	return LimitWindow(v)
}

// CanonicalCatalogueWindow is an exact canonical-leaf match in the flat
// models.dev models.json catalogue, with an unambiguous family fallback.
func CanonicalCatalogueWindow(catalogue obj, modelID, ownedBy string) *Limit {
	var entry obj
	if ownedBy != "" {
		v := catalogue[ownedBy+"/"+modelID]
		entry = objOf(v)
	}
	if entry == nil {
		suffix := "/" + modelID
		var matches []obj
		for _, k := range slices.Sorted(maps.Keys(catalogue)) {
			v := catalogue[k]
			if o := objOf(v); o != nil && strings.HasSuffix(k, suffix) {
				matches = append(matches, o)
			}
		}
		if len(matches) > 1 {
			return nil // An alias must not override conflicting exact identities.
		}
		if len(matches) == 1 {
			entry = matches[0]
		}
	}
	if entry != nil {
		if w := windowOf(entry); w != nil {
			return w
		}
		// An exact match with no usable window falls through to the family tier.
		entry = nil
	}
	var family []obj
	for _, k := range slices.Sorted(maps.Keys(catalogue)) {
		v := catalogue[k]
		o := objOf(v)
		if o == nil {
			continue
		}
		if fam := o["family"]; fam == modelID && (ownedBy == "" || strings.HasPrefix(k, ownedBy+"/")) {
			family = append(family, o)
		}
	}
	values := map[int64]bool{}
	for _, o := range family {
		if w := windowOf(o); w != nil {
			values[w.Value] = true
		}
	}
	if len(values) != 1 {
		return nil
	}
	for _, o := range family {
		if windowOf(o) != nil {
			entry = o
			break
		}
	}
	return windowOf(entry)
}

// ExecDir is the directory of the running binary, symlinks resolved.
func ExecDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(exe)
}

// ShareDir holds the bundled catalogue; a var so tests can point it elsewhere.
var ShareDir = func() string { return filepath.Join(ExecDir(), "..", "share") }

func bundledWindow(model string) (int64, bool) {
	b, err := os.ReadFile(filepath.Join(ShareDir(), "model-catalogue.json"))
	if err != nil {
		return 0, false
	}
	v, err := parseJSON(b)
	top := objOf(v)
	if err != nil || top == nil {
		return 0, false
	}
	models := objOf(top["models"])
	if models == nil {
		return 0, false
	}
	if found := CanonicalCatalogueWindow(models, model, ""); found != nil {
		return found.Value, true
	}
	return 0, false
}

func stated(row obj) (int64, bool) {
	for _, f := range DeclaredFields {
		v := row[f]
		if n, ok := posInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

func endpointWindow(model string) (int64, bool) {
	text, ok := os.LookupEnv("MODEL_WINDOW_ENDPOINT_JSON")
	if !ok {
		text = "[]"
	}
	v, err := parseJSON([]byte(text))
	if err != nil {
		return 0, false
	}
	rows, _ := v.([]any)
	for _, r := range rows {
		if row := objOf(r); row != nil {
			if id := row["id"]; id == model {
				return stated(row)
			}
		}
	}
	return 0, false
}

// Record is what resolve answers: the window, the id to send, and where it came from.
type Record struct {
	Window   int64
	SendID   string
	Source   string
	Unprobed bool
	Budget   int64
}

// JSON is the record as json.dumps prints it.
func (r Record) JSON() string {
	return jsonString(struct {
		Window   int64  `json:"window"`
		SendID   string `json:"send_id"`
		Source   string `json:"source"`
		Unprobed bool   `json:"unprobed,omitempty"`
		Budget   int64  `json:"budget,omitempty"`
	}{r.Window, r.SendID, r.Source, r.Unprobed, r.Budget})
}

// Fields is the record as "<window>\t<source>\t<budget>\t<send_id>" for the shell.
// The window is blank for an unprobed model: the fallback budget is the fact.
func (r Record) Fields() []string {
	window, budget := strconv.FormatInt(r.Window, 10), ""
	if r.Unprobed {
		window, budget = "", strconv.FormatInt(r.Budget, 10)
	}
	return []string{window, r.Source, budget, r.SendID}
}

func routed(fields map[string]string) bool {
	switch fields["type"] {
	case "apikey", "proxy", "provider":
		return true
	}
	return false
}

// Resolve sizes model on profile: env pin, manual pin, discovered window,
// endpoint metadata, bundled catalogue, then the unknown budget.
func Resolve(profile, model string) Record {
	if pin, ok := Positive(os.Getenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS")); ok {
		return Record{Window: pin, SendID: model, Source: "conf"}
	}
	bare := strings.TrimSuffix(model, "[1m]")
	fields := map[string]string{}
	if path, err := ProfilePath(profile); err == nil {
		if _, f, err := ReadConf(path); err == nil {
			fields = f
		}
	}
	// The served id sizes the session; the requested id stays what the client sees.
	target := MappedTarget(fields, bare)
	manual := ManualValues(fields)
	// An explicit pin wins, on the requested id first and then on the served id.
	window, have := manual[bare]
	if !have {
		window, have = manual[target]
	}
	source := "conf-manual"
	if !have {
		discovered := DiscoveredValues(fields)
		matched, found := "", false
		if _, ok := discovered[target]; ok {
			matched, found = target, true
		} else {
			for _, p := range slices.Sorted(maps.Keys(discovered)) {
				if glob(target, p) {
					matched, found = p, true
					break
				}
			}
		}
		if found {
			window, have = discovered[matched]
			have = true
			if tagged, ok := DiscoveredSources(fields)[matched]; ok {
				s := tagged["source"]
				source = "conf-discovered-" + s.(string)
			} else {
				source = "conf-discovered"
			}
		}
	}
	if !have {
		window, have = endpointWindow(target)
		source = "endpoint"
	}
	if !have && routed(fields) {
		window, have = bundledWindow(target)
		source = "catalogue-bundled"
	}
	rec := Record{Window: window, SendID: bare, Source: source}
	if !have {
		rec.Window, rec.Source = CCDefaultWindow, "default"
		rec.Unprobed, rec.Budget = true, UnknownBudget
	}
	if strings.HasPrefix(bare, "claude-") && rec.Window >= oneM {
		rec.SendID = bare + "[1m]"
	}
	return rec
}
