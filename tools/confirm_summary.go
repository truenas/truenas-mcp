package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/redact"
)

// ConfirmSummary is the human-facing headline and key facts shown in
// elicitation prompts and in dry-run previews (confirm_summary). It is
// not part of the confirmation-token plan hash: the token stays bound to
// Method, CallParams and Bind only.
type ConfirmSummary struct {
	Headline string   `json:"headline"`
	Facts    []string `json:"facts,omitempty"`
}

// confirmSummaryBudget is the maximum size of the displayed headline and
// facts, in bytes. Oversized change sets are cut with moreChangesNotShown.
const confirmSummaryBudget = 2 * 1024

const moreChangesNotShown = "more changes not shown"

func (s *ConfirmSummary) empty() bool {
	return s == nil || strings.TrimSpace(s.Headline) == ""
}

func newConfirmSummary(headline string, facts ...string) *ConfirmSummary {
	return &ConfirmSummary{Headline: headline, Facts: facts}
}

// forDisplay redacts the headline and facts and cuts the result to
// confirmSummaryBudget, appending moreChangesNotShown when anything is
// dropped or a fact is truncated.
func (s *ConfirmSummary) forDisplay() *ConfirmSummary {
	if s == nil {
		return nil
	}
	out := &ConfirmSummary{Headline: strings.TrimSpace(redact.Text(s.Headline))}
	used := len(out.Headline)
	notice := moreChangesNotShown
	for i, raw := range s.Facts {
		f := strings.TrimSpace(redact.Text(raw))
		if f == "" {
			continue
		}
		add := 1 + len(f)
		remain := confirmSummaryBudget - used
		moreLeft := i < len(s.Facts)-1
		needNotice := 0
		if moreLeft || add > remain {
			needNotice = 1 + len(notice)
		}
		if add+needNotice > remain {
			room := remain - needNotice - 1
			if room > 3 {
				out.Facts = append(out.Facts, truncateBytes(f, room-3)+"...")
				used += 1 + room
			}
			out.Facts = append(out.Facts, notice)
			return out
		}
		out.Facts = append(out.Facts, f)
		used += add
	}
	return out
}

func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

// requireConfirmSummary is nil when a destructive plan has a headline the
// human can approve. startElicit fails closed otherwise, rather than
// deriving a prompt from Actions[0].
func requireConfirmSummary(name string, plan *writePlan) error {
	if plan == nil || plan.ConfirmSummary.empty() {
		return fmt.Errorf("%s was NOT executed: missing confirmation summary; nothing was executed", name)
	}
	return nil
}

// confirmFact formats key: value after running the value through
// redact.Value under that key, so password and bindpw (and any other
// sensitive name) are replaced with redact.Marker.
func confirmFact(key string, value interface{}) string {
	return key + ": " + confirmShown(key, value)
}

func confirmShown(key string, value interface{}) string {
	wrapped := redact.Value(map[string]interface{}{key: value})
	v := value
	if m, ok := wrapped.(map[string]interface{}); ok {
		v = m[key]
	}
	b, err := json.Marshal(v)
	if err != nil {
		return redact.Marker
	}
	return string(b)
}

func confirmBoolFact(key string, v bool) string {
	return confirmFact(key, v)
}

// updateAppConfirmFacts lists top-level keys whose values differ from the
// current config, then one fact per changed leaf field (path: old -> new),
// so an unchanged field inside a resent object is not shown as part of the
// change, and every storage slot whose type or location differs. Omitted
// current keys are unchanged under a shallow merge and are not listed.
// Shown values come from the redacted current and next values, so a field
// that is secret in context (an env entry's value) stays masked, and a
// changed secret is reported without its value.
func updateAppConfirmFacts(current, next map[string]interface{}) []string {
	if current == nil {
		current = map[string]interface{}{}
	}
	if next == nil {
		next = map[string]interface{}{}
	}
	shownCur := redact.Value(deepCopyValue(current))
	shownNext := redact.Value(deepCopyValue(next))
	keys := make([]string, 0, len(next))
	for k := range next {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var changed []string
	var valueFacts []string
	for _, k := range keys {
		if k == "storage" {
			continue
		}
		if jsonValuesEqual(current[k], next[k]) {
			continue
		}
		changed = append(changed, k)
		cur, curOK := current[k]
		valueFacts = leafDiffFacts(valueFacts, valuePath{k},
			leafSide{cur, childShown(shownCur, k), curOK},
			leafSide{next[k], childShown(shownNext, k), true})
	}
	sort.SliceStable(valueFacts, func(i, j int) bool { return len(valueFacts[i]) < len(valueFacts[j]) })
	var facts []string
	if len(changed) > 0 {
		facts = append(facts, "changing keys: "+strings.Join(changed, ", "))
	}
	facts = append(facts, valueFacts...)
	if _, ok := next["storage"]; ok {
		facts = append(facts, storageDiffFacts(asStringMap(current["storage"]), asStringMap(next["storage"]))...)
	}
	return facts
}

// leafSide is one side of a field-level diff: the raw value (compared),
// the redacted value at the same path (shown) and whether it is present.
type leafSide struct {
	raw, shown interface{}
	ok         bool
}

// childShown is the redacted value under key k (a string or an int index)
// of shown. Under a value redaction replaced wholesale, every child is
// shown as the marker.
func childShown(shown interface{}, k interface{}) interface{} {
	switch s := shown.(type) {
	case map[string]interface{}:
		if key, ok := k.(string); ok {
			return s[key]
		}
	case []interface{}:
		if i, ok := k.(int); ok && i >= 0 && i < len(s) {
			return s[i]
		}
	case string:
		if s == redact.Marker {
			return redact.Marker
		}
	}
	return nil
}

// leafDiffFacts appends a "path: old -> new" fact for each leaf that
// differs between a and b. Objects are compared key by key and lists of
// the same length index by index; anything else is a leaf.
func leafDiffFacts(facts []string, p valuePath, a, b leafSide) []string {
	if a.ok == b.ok && (!a.ok || jsonValuesEqual(a.raw, b.raw)) {
		return facts
	}
	am, aMap := a.raw.(map[string]interface{})
	bm, bMap := b.raw.(map[string]interface{})
	if a.ok && b.ok && aMap && bMap {
		names := map[string]bool{}
		for k := range am {
			names[k] = true
		}
		for k := range bm {
			names[k] = true
		}
		for _, k := range sortedKeys(names) {
			av, aok := am[k]
			bv, bok := bm[k]
			facts = leafDiffFacts(facts, appendPath(p, k),
				leafSide{av, childShown(a.shown, k), aok},
				leafSide{bv, childShown(b.shown, k), bok})
		}
		return facts
	}
	al, aList := a.raw.([]interface{})
	bl, bList := b.raw.([]interface{})
	if a.ok && b.ok && aList && bList && len(al) == len(bl) {
		for i := range al {
			facts = leafDiffFacts(facts, appendPath(p, i),
				leafSide{al[i], childShown(a.shown, i), true},
				leafSide{bl[i], childShown(b.shown, i), true})
		}
		return facts
	}
	if a.ok && b.ok && jsonValuesEqual(a.shown, b.shown) {
		// Both sides are masked the same way: say it changed, not to what.
		return append(facts, p.String()+": changed (secret, not shown)")
	}
	return append(facts, p.String()+": "+leafShown(a)+" -> "+leafShown(b))
}

func leafShown(s leafSide) string {
	if !s.ok {
		return "(not set)"
	}
	b, err := json.Marshal(s.shown)
	if err != nil {
		return redact.Marker
	}
	return string(b)
}

func asStringMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func storageDiffFacts(current, next map[string]interface{}) []string {
	if next == nil {
		return nil
	}
	names := map[string]struct{}{}
	for k := range current {
		names[k] = struct{}{}
	}
	for k := range next {
		names[k] = struct{}{}
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var facts []string
	for _, name := range keys {
		slot := "storage." + name
		if name == "additional_storage" {
			facts = append(facts, additionalStorageDiffFacts(slot, current[name], next[name])...)
			continue
		}
		facts = append(facts, storageEntryDiffFact(slot, asStringMap(current[name]), asStringMap(next[name]))...)
	}
	return facts
}

func additionalStorageDiffFacts(slot string, current, next interface{}) []string {
	curList, _ := current.([]interface{})
	nextList, _ := next.([]interface{})
	n := len(curList)
	if len(nextList) > n {
		n = len(nextList)
	}
	var facts []string
	for i := 0; i < n; i++ {
		var cur, nxt map[string]interface{}
		if i < len(curList) {
			cur = asStringMap(curList[i])
		}
		if i < len(nextList) {
			nxt = asStringMap(nextList[i])
		}
		facts = append(facts, storageEntryDiffFact(fmt.Sprintf("%s[%d]", slot, i), cur, nxt)...)
	}
	return facts
}

func storageEntryDiffFact(slot string, current, next map[string]interface{}) []string {
	switch {
	case current == nil && next == nil:
		return nil
	case next == nil:
		return []string{slot + ": removed"}
	case current == nil:
		return []string{fmt.Sprintf("%s: new type %s, location %s", slot, storageEntryType(next), storageEntryLocation(next))}
	case storageLocationUnchanged(current, next):
		return nil
	default:
		return []string{fmt.Sprintf("%s: type %s -> %s, location %s -> %s",
			slot, storageEntryType(current), storageEntryType(next),
			storageEntryLocation(current), storageEntryLocation(next))}
	}
}

// directoryServiceTargetFacts are the domain or server identity fields from
// a configure_directory_service payload. Credential secrets are never
// included; domain, hostname, target_server and server_urls are shown
// after redact.Value.
func directoryServiceTargetFacts(payload map[string]interface{}) []string {
	cfg := asStringMap(payload["configuration"])
	if cfg == nil {
		return nil
	}
	var facts []string
	for _, key := range []string{"domain", "hostname", "target_server", "server_urls"} {
		if v, ok := cfg[key]; ok && v != nil && v != "" {
			facts = append(facts, confirmFact(key, v))
		}
	}
	return facts
}

func directoryStatusLabel(status *DirectoryServiceStatus) string {
	if status == nil || status.Status == nil {
		return ""
	}
	if s, ok := status.Status["status"].(string); ok {
		return s
	}
	return ""
}
