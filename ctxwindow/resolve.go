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
	if n, ok := asPosInt(v); ok {
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
		entry = asObj(v)
	}
	if entry == nil {
		suffix := "/" + modelID
		var matches []obj
		for _, k := range slices.Sorted(maps.Keys(catalogue)) {
			v := catalogue[k]
			if o := asObj(v); o != nil && strings.HasSuffix(k, suffix) {
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
		o := asObj(v)
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

// Inputs is what Resolve reads outside the launched environment and that cannot
// change while one caller runs: a conf's fields, the endpoint's declared
// windows, and for a routed profile the bundled catalogue. Build it once and
// resolve every model, so a caller resolving many models reads and parses each
// input once instead of once per model (#424).
type Inputs struct {
	fields        map[string]string
	endpoints     []any
	haveEndpoints bool
	catalogue     obj
	haveCatalogue bool
	mapping       obj
	haveMapping   bool
	doc           obj
	haveDoc       bool
}

// NewInputs is a resolver over conf fields its caller has already read.
func NewInputs(fields map[string]string) *Inputs { return &Inputs{fields: fields} }

// LoadInputs reads the conf at path once and returns its resolver inputs.
func LoadInputs(path string) *Inputs {
	var fields map[string]string
	if _, f, err := ReadConf(path); err == nil {
		fields = f
	}
	return NewInputs(fields)
}

// Preload reads and parses every input Resolve can consult: the endpoint env
// JSON, the model map and discovered record in the fields, and, for a routed
// profile, the bundled catalogue. Afterwards Resolve touches no file,
// environment variable or field text, so a caller resolving many models calls
// it once before its loop.
func (in *Inputs) Preload() {
	in.endpointRows()
	in.modelMap()
	in.discoveredDoc()
	if routed(in.fields) {
		in.catalogueModels()
	}
}

// modelMap is the conf's model_map parsed once, nil when it holds no mapping.
func (in *Inputs) modelMap() obj {
	if !in.haveMapping {
		in.haveMapping = true
		in.mapping = parseModelMap(in.fields)
	}
	return in.mapping
}

// mappedTarget is the id the shim serves for bare, from the cached model map.
func (in *Inputs) mappedTarget(bare string) string {
	return mappedTarget(in.fields, in.modelMap(), bare)
}

// discoveredDoc is the route-bound discovered record parsed once; nil when its
// text is not a JSON object or the route changed.
func (in *Inputs) discoveredDoc() obj {
	if !in.haveDoc {
		in.haveDoc = true
		in.doc = discoveredDoc(in.fields)
	}
	return in.doc
}

// discoveredValues is the route-bound window table from the cached doc.
func (in *Inputs) discoveredValues() map[string]int64 { return valuesOf(in.discoveredDoc()) }

// discoveredSources is the provenance table from the cached doc.
func (in *Inputs) discoveredSources() map[string]obj { return sourcesOf(in.discoveredDoc()) }

func (in *Inputs) endpointRows() []any {
	if !in.haveEndpoints {
		in.haveEndpoints = true
		text, ok := os.LookupEnv("MODEL_WINDOW_ENDPOINT_JSON")
		if !ok {
			text = "[]"
		}
		if v, err := parseJSON([]byte(text)); err == nil {
			in.endpoints, _ = v.([]any)
		}
	}
	return in.endpoints
}

func (in *Inputs) endpointWindow(model string) (int64, bool) {
	for _, r := range in.endpointRows() {
		if row := asObj(r); row != nil {
			if id := row["id"]; id == model {
				return stated(row)
			}
		}
	}
	return 0, false
}

func (in *Inputs) catalogueModels() obj {
	if !in.haveCatalogue {
		in.haveCatalogue = true
		if b, err := os.ReadFile(filepath.Join(ShareDir(), "model-catalogue.json")); err == nil {
			if v, err := parseJSON(b); err == nil {
				if top := asObj(v); top != nil {
					in.catalogue = asObj(top["models"])
				}
			}
		}
	}
	return in.catalogue
}

func (in *Inputs) bundledWindow(model string) (int64, bool) {
	models := in.catalogueModels()
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
		if n, ok := asPosInt(v); ok {
			return n, true
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
// endpoint metadata, bundled catalogue, then the unknown budget. A caller
// resolving many models builds Inputs once and calls its Resolve instead.
func Resolve(profile, model string) Record {
	if pin, ok := Positive(os.Getenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS")); ok {
		return Record{Window: pin, SendID: model, Source: "conf"}
	}
	if path, err := ProfilePath(profile); err == nil {
		return LoadInputs(path).Resolve(model)
	}
	return NewInputs(nil).Resolve(model)
}

// Resolve sizes model from the preloaded inputs. It ignores the
// CLAUDE_CODE_MAX_CONTEXT_TOKENS pin: that belongs to Resolve's environment.
func (in *Inputs) Resolve(model string) Record {
	fields := in.fields
	bare := strings.TrimSuffix(model, "[1m]")
	// The served id sizes the session; the requested id stays what the client sees.
	target := in.mappedTarget(bare)
	manual := ManualValues(fields)
	// An explicit pin wins, on the requested id first and then on the served id.
	window, have := manual[bare]
	if !have {
		window, have = manual[target]
	}
	source := "conf-manual"
	if !have {
		discovered := in.discoveredValues()
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
			if tagged, ok := in.discoveredSources()[matched]; ok {
				s := tagged["source"]
				source = "conf-discovered-" + s.(string)
			} else {
				source = "conf-discovered"
			}
		}
	}
	if !have {
		window, have = in.endpointWindow(target)
		source = "endpoint"
	}
	if !have && routed(fields) {
		window, have = in.bundledWindow(target)
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
