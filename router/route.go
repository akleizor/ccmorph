// Package router is ccmorph's optional subagent router, off by default
// (`ccmorph router on`). It spreads subagent work across the user's
// subscription quotas, then matches the task's kind and tier to a served model.
// docs/ARCHITECTURE.md#subagent-router is its rulebook.
package router

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var Kinds = []string{"writing", "search", "x", "mechanical", "code", "design", "image_gen", "vision"}

// Tiers in order, each with the effort it runs at.
var Tiers = []string{"small", "mid", "top"}
var Efforts = map[string]string{"small": "low", "mid": "medium", "top": "high"}

type Task struct {
	Kind   string `json:"kind"`
	Tier   string `json:"tier"`
	Source string `json:"source"`
	// Lang is "zh" for writing whose text is Chinese; its families then come
	// from the writing_zh order.
	Lang string `json:"lang,omitempty"`
}

// familyOrder is the kind's family preference: Chinese writing takes
// writing_zh when one is set, every other task its kind's own order.
func familyOrder(kinds map[string][]string, kind, lang string) []string {
	if zh := kinds["writing_zh"]; kind == "writing" && lang == "zh" && len(zh) > 0 {
		return zh
	}
	return kinds[kind]
}

// Window is one quota window: Used is the share spent (0..1), times are Unix seconds.
type Window struct {
	Name     string   `json:"name"`
	Used     float64  `json:"used"`
	ResetsAt float64  `json:"resets_at"`
	Length   float64  `json:"length"`
	Models   []string `json:"models,omitempty"`
}

type Profile struct {
	Name             string
	Class            string // free, subscription, budget, api
	Models           []string
	Windows          []Window
	Reserved         bool
	Enabled          bool
	UnavailableUntil float64
}

// Label is what the router knows of one served model.
type Label struct {
	Family   string  `json:"family"`
	Tier     string  `json:"tier"`
	Vision   *bool   `json:"vision,omitempty"`
	ImageGen bool    `json:"image_gen,omitempty"`
	Price    float64 `json:"-"` // blended $ per 1M tokens; +Inf when unknown
}

type Option struct {
	Profile      string  `json:"profile"`
	Model        string  `json:"model"`
	Effort       string  `json:"effort"`
	Class        string  `json:"class"`
	Family       string  `json:"family"`
	Tier         string  `json:"tier"`
	Slack        float64 `json:"slack"`
	Rank         int     `json:"rank"`
	QuotaUnknown bool    `json:"quota_unknown"`
	Price        float64 `json:"-"`
}

// Pin is what the caller or config fixed: a profile, a model, an allowed set
// for the tier (Only, nil when unset) and a preferred order.
type Pin struct {
	Profile, Model, Effort string
	Only                   []string
	Order                  []string
	// AnyTier keeps every usable model whatever its tier or family rank: the
	// shadow picker's pool. Usability (disabled, quota, modality) still filters.
	AnyTier bool
}

func drawsOn(w Window, model string) bool {
	return model == "" || len(w.Models) == 0 || slices.Contains(w.Models, model)
}

// Slack is elapsed minus used, in percentage points, for the tightest active
// window the model draws on; 0 without a reading.
func Slack(p Profile, now float64, model string) float64 {
	best, any := math.Inf(1), false
	for _, w := range p.Windows {
		if !drawsOn(w, model) || w.Length <= 0 || w.ResetsAt <= now {
			continue
		}
		elapsed := math.Max(0, math.Min(1, 1-(w.ResetsAt-now)/w.Length))
		best, any = math.Min(best, elapsed-w.Used), true
	}
	if !any {
		return 0
	}
	return 100 * best
}

var visionWord = regexp.MustCompile(`(?:^|[-_/])(?:vl|vision)(?:$|[-_/])`)

func visionCapable(model string, l Label) bool {
	if l.Vision != nil {
		return *l.Vision
	}
	switch l.Family {
	case "claude", "gpt", "gemini", "gemini-flash":
		return true
	}
	return visionWord.MatchString(strings.ToLower(model))
}

var imageWord = regexp.MustCompile(`(?i)image|dall-e|imagen`)

// Capped stops a kind whose quality does not grow with model size at its tier.
func Capped(t Task, caps map[string]string) Task {
	c, ok := caps[t.Kind]
	if ok && slices.Index(Tiers, c) >= 0 && slices.Index(Tiers, t.Tier) > slices.Index(Tiers, c) {
		t.Tier = c
	}
	return t
}

var digits = regexp.MustCompile(`\d+`)

// newer compares served IDs by their numbers, then by name.
func newer(a, b string) bool {
	na := digits.FindAllString(strings.TrimSuffix(a, "[1m]"), -1)
	nb := digits.FindAllString(strings.TrimSuffix(b, "[1m]"), -1)
	for i := 0; i < len(na) && i < len(nb); i++ {
		x, _ := strconv.Atoi(na[i])
		y, _ := strconv.Atoi(nb[i])
		if x != y {
			return x > y
		}
	}
	if len(na) != len(nb) {
		return len(na) > len(nb)
	}
	return a > b
}

// Options is one newest served model per family on each eligible profile, with
// the reason every other model was dropped.
func Options(t Task, profiles []Profile, labels func(profile, model string) (Label, bool),
	kinds map[string][]string, now float64, pin Pin) ([]Option, []string, error) {
	if !slices.Contains(Kinds, t.Kind) || Efforts[t.Tier] == "" {
		return nil, nil, fmt.Errorf("task needs a valid kind and tier")
	}
	order := familyOrder(kinds, t.Kind, t.Lang)
	capable := order
	if len(capable) == 0 && t.Kind == "x" {
		capable = []string{"grok"}
	}
	chosen := pin.Only != nil || pin.Model != ""
	var out []Option
	var dropped []string
	for _, p := range profiles {
		switch {
		case p.Reserved:
			dropped = append(dropped, p.Name+": reserved")
			continue
		case !p.Enabled:
			dropped = append(dropped, p.Name+": disabled")
			continue
		case p.UnavailableUntil > now:
			dropped = append(dropped, p.Name+": unavailable")
			continue
		}
		if pin.Profile != "" && p.Name != pin.Profile {
			continue
		}
		type pick struct{ model, family string }
		newest := map[string]pick{}
		var families []string
		for _, m := range p.Models {
			if pin.Model != "" && m != pin.Model || pin.Only != nil && !slices.Contains(pin.Only, m) {
				continue
			}
			l, ok := labels(p.Name, m)
			reason := ""
			switch {
			case !ok || l.Family == "" || Efforts[l.Tier] == "":
				reason = "no family/tier label"
			case l.Tier != t.Tier && !chosen && !pin.AnyTier:
				reason = "not " + t.Tier + " tier"
			case (t.Kind == "search" || t.Kind == "x" || t.Kind == "image_gen") && !slices.Contains(capable, l.Family):
				reason = "no " + t.Kind + " capability in config"
			case t.Kind == "image_gen" && !l.ImageGen && !imageWord.MatchString(m):
				reason = "cannot generate images"
			case t.Kind == "vision" && !visionCapable(m, l):
				reason = "cannot read images"
			// Mechanical work wants the cheapest families, which many profiles
			// do not serve at all: its order ranks a top pick, never empties it.
			case t.Tier == "top" && t.Kind != "mechanical" && !pin.AnyTier && len(order) > 0 && !slices.Contains(order[:min(2, len(order))], l.Family):
				reason = "outside the kind's top two families"
			case poolSpent(p, m, now):
				reason = "its quota pool is used up"
			}
			if reason != "" {
				dropped = append(dropped, p.Name+"/"+m+": "+reason)
				continue
			}
			// One model per family, the newest; across tiers a family's small and
			// top lines are different models, not versions of one.
			key := l.Family
			if pin.AnyTier {
				key += "/" + l.Tier
			}
			prev, seen := newest[key]
			// The user's order outranks a newer version of the same family:
			// when both are ordered, the earlier entry wins; a newer model
			// only decides between two the order says nothing about.
			mPref, pPref := preferred(m, pin.Order), preferred(prev.model, pin.Order)
			better := !seen || mPref && !pPref ||
				mPref && pPref && slices.Index(pin.Order, m) < slices.Index(pin.Order, prev.model) ||
				!mPref && !pPref && newer(m, prev.model)
			if !better {
				dropped = append(dropped, p.Name+"/"+m+": older served model")
				continue
			}
			if seen {
				dropped = append(dropped, p.Name+"/"+prev.model+": older served model")
			} else {
				families = append(families, key)
			}
			newest[key] = pick{m, l.Family}
		}
		for _, k := range families {
			m, f := newest[k].model, newest[k].family
			l, _ := labels(p.Name, m)
			rank := slices.Index(order, f) + 1
			if rank == 0 {
				rank = len(order) + 1
			}
			price := l.Price
			if price == 0 || math.IsNaN(price) {
				price = math.Inf(1)
			}
			out = append(out, Option{Profile: p.Name, Model: m, Effort: Efforts[t.Tier], Class: p.Class,
				Family: f, Tier: t.Tier, Slack: Slack(p, now, m), Rank: rank,
				QuotaUnknown: len(p.Windows) == 0, Price: price})
		}
	}
	return out, dropped, nil
}

func preferred(m string, order []string) bool { return slices.Contains(order, m) }

func poolSpent(p Profile, m string, now float64) bool {
	for _, w := range p.Windows {
		if len(w.Models) > 0 && slices.Contains(w.Models, m) && w.Used >= 1 && w.ResetsAt > now {
			return true
		}
	}
	return false
}

// classRank: free first (it costs nothing), then perishable quota behind pace,
// ahead of pace, then paid API.
func classRank(o Option) int {
	if o.Class == "free" {
		return 0
	}
	// A subscription without a reading counts as behind pace: the vendor's refusal marks it unavailable.
	behind := o.Slack > 0 || o.QuotaUnknown
	// A monthly budget expires like a subscription window once its spend is read.
	perishable := o.Class == "subscription" || o.Class == "budget" && !o.QuotaUnknown
	switch {
	case perishable && behind:
		return 1
	case perishable:
		return 2
	}
	return 3
}

// Choose sorts the options: the user's order first, then class, largest slack,
// cheapest, preference rank. The first one is the pick. Between two free
// options the kind's preference comes before list price: a free model costs
// nothing, so its list price must not override the kind's family.
func Choose(opts []Option, order []string) []Option {
	out := slices.Clone(opts)
	pos := func(m string) int {
		if i := slices.Index(order, m); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(out, func(a, b Option) int {
		for _, d := range []float64{
			float64(pos(a.Model) - pos(b.Model)),
			float64(classRank(a) - classRank(b)),
			b.Slack - a.Slack,
			freeRank(a, b),
			cmpInf(a.Price, b.Price),
			float64(a.Rank - b.Rank),
		} {
			if d != 0 {
				return sign(d)
			}
		}
		return strings.Compare(a.Profile+"\x00"+a.Model, b.Profile+"\x00"+b.Model)
	})
	return out
}

// freeRank orders two free options by the kind's preference and leaves any
// other pair to price.
func freeRank(a, b Option) float64 {
	if a.Class != "free" || b.Class != "free" {
		return 0
	}
	return float64(a.Rank - b.Rank)
}

func cmpInf(a, b float64) float64 {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func sign(d float64) int {
	if d < 0 {
		return -1
	}
	return 1
}
