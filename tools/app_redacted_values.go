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
	var kept []valuePath
	out, err := restoreRedactedValue(values, current, current != nil, nil, &kept)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].String() < kept[j].String() })
	m, _ := out.(map[string]interface{})
	return m, kept, nil
}

func restoreRedactedValue(v, cur interface{}, curOK bool, path valuePath, kept *[]valuePath) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		if x == nil {
			return x, nil
		}
		curMap, _ := cur.(map[string]interface{})
		out := make(map[string]interface{}, len(x))
		for k, child := range x {
			c, ok := curMap[k]
			r, err := restoreRedactedValue(child, c, curOK && ok, appendPath(path, k), kept)
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
			r, err := restoreRedactedValue(child, c, aligned, appendPath(path, i), kept)
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
				return nil, fmt.Errorf("values.%s is the redaction marker %s, but the current app config has no stored secret at that path to keep; send the real value instead (omitting it may clear it)", path, redact.Marker)
			}
			*kept = append(*kept, path)
			return stored, nil
		}
		if curOK && ok && stored == x {
			return x, nil
		}
		return nil, fmt.Errorf("values.%s contains the redaction marker %s inside a longer value; only a value that is exactly the marker can be kept at its stored value, so send the full real value instead", path, redact.Marker)
	default:
		return v, nil
	}
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
