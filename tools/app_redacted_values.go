package tools

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/truenas/truenas-mcp/redact"
)

// get_app_config output is redacted, so a secret nested in app values (for
// example an auth key inside a settings object) comes back as
// redact.Marker. app.update replaces whole top-level values, so a caller
// that changes one nested field must send the whole object back, and the
// only value it has for the secret is the marker. Sending the marker would
// overwrite the stored secret with that literal string, so update_app puts
// the stored value back first.

// valuePath is a location in app values: string map keys and int list
// indexes.
type valuePath []interface{}

func (p valuePath) String() string {
	var b strings.Builder
	for _, seg := range p {
		switch s := seg.(type) {
		case int:
			b.WriteString("[" + strconv.Itoa(s) + "]")
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, s)
		}
	}
	return b.String()
}

// restoreRedactedValues returns a copy of values in which every string that
// is exactly redact.Marker is replaced with the non-empty string stored at
// the same path in current, plus the paths that were replaced (sorted by
// name). Map keys are matched by name; a list element is matched only when
// the current value at that path is a list of the same length. A marker with
// no stored string at its path, or a string that embeds a marker and differs
// from the stored value, is an error naming the path, so the marker is never
// written to the app. values and current are not modified.
func restoreRedactedValues(values, current map[string]interface{}) (map[string]interface{}, []valuePath, error) {
	return restoreRedactedValuesFrom(values, current, restoreLabels{prefix: "values.", stored: "the current app config"})
}

// restoreLabels name, in errors, the argument a path is under (prefix)
// and where the stored values come from.
type restoreLabels struct {
	prefix, stored string
}

// restoreRedactedValuesFrom is restoreRedactedValues with the error labels
// of the calling tool.
func restoreRedactedValuesFrom(values, current map[string]interface{}, l restoreLabels) (map[string]interface{}, []valuePath, error) {
	var kept []valuePath
	out, err := restoreRedactedValue(values, current, current != nil, nil, &kept, l)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].String() < kept[j].String() })
	m, _ := out.(map[string]interface{})
	return m, kept, nil
}

func restoreRedactedValue(v, cur interface{}, curOK bool, path valuePath, kept *[]valuePath, l restoreLabels) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		if x == nil {
			return x, nil
		}
		curMap, _ := cur.(map[string]interface{})
		out := make(map[string]interface{}, len(x))
		for k, child := range x {
			c, ok := curMap[k]
			r, err := restoreRedactedValue(child, c, curOK && ok, appendPath(path, k), kept, l)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		if x == nil {
			return x, nil
		}
		curList, ok := cur.([]interface{})
		aligned := curOK && ok && len(curList) == len(x)
		out := make([]interface{}, len(x))
		for i, child := range x {
			var c interface{}
			if aligned {
				c = curList[i]
			}
			r, err := restoreRedactedValue(child, c, aligned, appendPath(path, i), kept, l)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		if !strings.Contains(x, redact.Marker) {
			return x, nil
		}
		stored, ok := cur.(string)
		if x == redact.Marker {
			if !curOK || !ok || stored == "" {
				return nil, fmt.Errorf("%s%s is the redaction marker %s, but %s has no non-empty text value at that path to keep; send the real value instead (omitting it may clear it)", l.prefix, path, redact.Marker, l.stored)
			}
			*kept = append(*kept, path)
			return stored, nil
		}
		if curOK && ok && stored == x {
			return x, nil
		}
		return nil, fmt.Errorf("%s%s contains the redaction marker %s inside a longer value; only a value that is exactly the marker can be kept at its stored value, so send the full real value instead", l.prefix, path, redact.Marker)
	default:
		return v, nil
	}
}

// checkKeptValues refuses a kept path unless keeping it is safe:
//   - get_app_config redacts the stored value there (redact.Value of current
//     has the marker at that path), so only real redacted secrets are kept;
//   - the outgoing values still redact it there after substitution and
//     storage processing, so a renamed or reordered sibling (for example an
//     env entry name) cannot move the secret out from under redaction;
//   - every enclosing entry keeps its identity (its name, key, variable,
//     username or principal field), so a secret is not handed to a
//     different entry even when the new name is masked too (DB_PASSWORD
//     renamed to API_TOKEN). This fails closed: a legitimate rename that
//     keeps the secret is refused too, and the caller must send the real
//     value;
//   - no enclosing storage entry changed its type or location, so a stored
//     share credential is never sent to a different server or path (the
//     same rule that limits filling omitted storage fields). An entry is
//     treated as storage when either the stored or the outgoing entry has
//     the storage shape.
func checkKeptValues(kept []valuePath, current, next map[string]interface{}) error {
	if len(kept) == 0 {
		return nil
	}
	if err := checkKeptRedacted(kept, current, next, "values.", "get_app_config"); err != nil {
		return err
	}
	for _, p := range kept {
		if err := checkKeptIdentity(p, current, next, "values."); err != nil {
			return err
		}
		for i := len(p) - 1; i > 0; i-- {
			prefix := p[:i]
			nextEntry, _ := getValuePath(next, prefix)
			nextMap, nextOK := nextEntry.(map[string]interface{})
			curEntry, _ := getValuePath(current, prefix)
			curMap, curOK := curEntry.(map[string]interface{})
			if !(nextOK && isStorageEntryShape(nextMap)) && !(curOK && isStorageEntryShape(curMap)) {
				continue
			}
			if !storageLocationUnchanged(curMap, nextMap) {
				return fmt.Errorf("values.%s is the redaction marker %s, but the storage entry %s changes type or location, so its stored value is not kept; send the real value for the new location", p, redact.Marker, prefix)
			}
		}
	}
	return nil
}

// checkKeptRedacted refuses a kept path unless the stored value there is
// one that source (the read tool the caller copied it from) shows as
// redact.Marker, and the outgoing value there would still be redacted
// after substitution. prefix is prepended to paths in errors.
func checkKeptRedacted(kept []valuePath, current, next map[string]interface{}, prefix, source string) error {
	shownCurrent := redact.Value(deepCopyValue(current))
	shownNext := redact.Value(deepCopyValue(next))
	for _, p := range kept {
		if v, ok := getValuePath(shownCurrent, p); !ok || v != redact.Marker {
			return fmt.Errorf("%s%s is the redaction marker %s, but %s does not redact the stored value at that path, so it cannot be kept; send the real value instead", prefix, p, redact.Marker, source)
		}
		if v, ok := getValuePath(shownNext, p); !ok || v != redact.Marker {
			return fmt.Errorf("%s%s is the redaction marker %s, but keeping the stored value there would leave it unredacted because the fields around it changed (for example a renamed or reordered entry); send the real value instead, or keep the surrounding fields unchanged", prefix, p, redact.Marker)
		}
	}
	return nil
}

// keptIdentityFields name the entry a value belongs to: the name of a
// {name|key|variable, value} pair, or who a credential or user entry is
// for.
var keptIdentityFields = []string{"name", "key", "variable", "username", "principal", "binddn", "credential_type"}

// checkKeptIdentity refuses kept path p when any object enclosing it (below
// the top level) has an identity field (keptIdentityFields) that was
// added, removed or changed between current and next.
func checkKeptIdentity(p valuePath, current, next map[string]interface{}, prefix string) error {
	for i := len(p) - 1; i > 0; i-- {
		entry := p[:i]
		curEntry, _ := getValuePath(current, entry)
		nextEntry, _ := getValuePath(next, entry)
		curMap, _ := curEntry.(map[string]interface{})
		nextMap, _ := nextEntry.(map[string]interface{})
		for _, f := range keptIdentityFields {
			cv, cok := curMap[f]
			nv, nok := nextMap[f]
			if cok != nok || (cok && !jsonValuesEqual(cv, nv)) {
				return fmt.Errorf("%s%s is the redaction marker %s, but its entry %s changes its %s field, so the stored value belongs to a different entry and is not kept; send the real value instead, or keep %s unchanged", prefix, p, redact.Marker, entry, f, f)
			}
		}
	}
	return nil
}

// findRedactionMarker returns the first path (in sorted key order) in v
// holding a string that is or contains redact.Marker.
func findRedactionMarker(v interface{}, path valuePath) (valuePath, bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		for _, k := range sortedKeys(x) {
			if p, ok := findRedactionMarker(x[k], appendPath(path, k)); ok {
				return p, true
			}
		}
	case []interface{}:
		for i, c := range x {
			if p, ok := findRedactionMarker(c, appendPath(path, i)); ok {
				return p, true
			}
		}
	case string:
		if strings.Contains(x, redact.Marker) {
			return path, true
		}
	}
	return nil, false
}

// isStorageEntryShape reports whether m looks like a catalog storage
// entry: a string type plus a <type>_config object.
func isStorageEntryShape(m map[string]interface{}) bool {
	typ := storageEntryType(m)
	if typ == "" {
		return false
	}
	_, ok := m[typ+"_config"].(map[string]interface{})
	return ok
}

func getValuePath(root interface{}, p valuePath) (interface{}, bool) {
	cur := root
	for _, seg := range p {
		switch c := cur.(type) {
		case map[string]interface{}:
			k, ok := seg.(string)
			if !ok {
				return nil, false
			}
			v, exists := c[k]
			if !exists {
				return nil, false
			}
			cur = v
		case []interface{}:
			idx, ok := seg.(int)
			if !ok || idx < 0 || idx >= len(c) {
				return nil, false
			}
			cur = c[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func appendPath(p valuePath, seg interface{}) valuePath {
	out := make(valuePath, len(p), len(p)+1)
	copy(out, p)
	return append(out, seg)
}

// maskValuePaths returns a deep copy of v with each of paths that exists in
// it set to redact.Marker. Use it on anything shown to the caller after
// restoreRedactedValues, so a restored secret is never displayed even if
// key-based redaction would not catch it.
func maskValuePaths(v map[string]interface{}, paths []valuePath) map[string]interface{} {
	if v == nil {
		return nil
	}
	out, _ := deepCopyValue(v).(map[string]interface{})
	for _, p := range paths {
		setValuePath(out, p)
	}
	return out
}

func setValuePath(root interface{}, p valuePath) {
	if len(p) == 0 {
		return
	}
	cur := root
	for i, seg := range p {
		last := i == len(p)-1
		switch c := cur.(type) {
		case map[string]interface{}:
			k, ok := seg.(string)
			if !ok {
				return
			}
			if _, exists := c[k]; !exists {
				return
			}
			if last {
				c[k] = redact.Marker
				return
			}
			cur = c[k]
		case []interface{}:
			idx, ok := seg.(int)
			if !ok || idx < 0 || idx >= len(c) {
				return
			}
			if last {
				c[idx] = redact.Marker
				return
			}
			cur = c[idx]
		default:
			return
		}
	}
}

func deepCopyValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		if x == nil {
			return x
		}
		out := make(map[string]interface{}, len(x))
		for k, c := range x {
			out[k] = deepCopyValue(c)
		}
		return out
	case []interface{}:
		if x == nil {
			return x
		}
		out := make([]interface{}, len(x))
		for i, c := range x {
			out[i] = deepCopyValue(c)
		}
		return out
	default:
		return v
	}
}

func valuePathNames(paths []valuePath) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p.String()
	}
	return out
}
