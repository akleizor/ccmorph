package util

import (
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
)

// ModelOrigin says where a profile's shown model came from; source is the
// conf's model_source value ("" when it has none).
func ModelOrigin(source string) string {
	if source == profile.ModelSourceSettings {
		return "from ~/.claude/settings.json"
	}
	return "saved in this profile"
}

// ServedSample is up to five chat model ids the listing serves once the
// profile's excluded_models is applied, sorted and deduplicated, then "…"
// when there are more; nil when the listing gives none or cannot be read.
func ServedSample(conf, listing string) []string {
	rows, ok := GwModels(conf, true, listing)
	if !ok {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if len(ids) == 5 {
			return append(ids, "…")
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// UnservedTierFix is the one command that repairs a tier alias the gateway does not serve.
func UnservedTierFix(name string) string {
	return "pick a model this gateway serves: ccmorph set " + name + " model -i"
}

// UnservedTier is why a launch cannot use a Claude model alias the profile
// maps to no gateway model, and its fix; served, when given, names ids it does serve.
func UnservedTier(name, tier, origin string, served []string) string {
	msg := tier + ", " + origin + ", is a Claude model alias this gateway does not serve; " + UnservedTierFix(name)
	if len(served) > 0 {
		msg += " (it serves " + strings.Join(served, ", ") + ")"
	}
	return msg
}

// NoModelReason is why a gateway profile with no saved model cannot start
// without a terminal, and the fix: the adapter's refusal and doctor's note.
func NoModelReason(name string) string {
	return name + " has no saved model, and this gateway's model list does not say which request format its models use; list them: ccmorph models " + name +
		", then save one: ccmorph set " + name + " model <model-id> (the next launch may send a few short test requests to learn its format and may incur a charge)"
}

// ExclusionsLeaveNone is whether excluded_models removes every chat model a
// readable listing serves — the state ProjectPicker refuses. False with no
// exclusions, an unreadable conf, or a body that is not a listing.
func ExclusionsLeaveNone(conf, listing string) bool {
	fields, err := readConf(conf)
	if err != nil || strings.Trim(fields["excluded_models"], ",") == "" {
		return false
	}
	if _, verdict := ctxwindow.ListingShapeBody(listing); verdict == ctxwindow.ListingNotAListing {
		return false
	}
	_, ok := GwModels(conf, true, listing)
	return !ok
}

// ExclusionsFix is the repair when excluded_models removes every served model.
func ExclusionsFix(name, conf string) string {
	return "shorten excluded_models in " + conf + " (ccmorph models " + name + " lists what the gateway serves)"
}

// ExclusionsRefusal is the launch's refusal for that state, and doctor's row.
func ExclusionsRefusal(name, conf string) string {
	return "no cached or served picker models remain after excluded_models, which removes every model this gateway serves; " + ExclusionsFix(name, conf)
}

// NoUsableModel is run's refusal when the gateway answers with a list that
// leaves no picker row: an empty list, or rows that are not chat models.
func NoUsableModel(name string) string {
	return "'" + name + "' endpoint returned no usable model list"
}
