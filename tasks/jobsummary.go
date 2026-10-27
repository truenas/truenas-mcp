package tasks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	// MaxJobResultBytes bounds a job result returned to the caller. Larger
	// results are summarised (app entries) or cut to a preview.
	MaxJobResultBytes = 4096
	// MaxJobTextChars bounds free-text job fields (error, exception).
	MaxJobTextChars = 2000
	// maxContainers bounds the per-container list in an app summary.
	maxContainers = 20
)

// TruncateText cuts s to at most max bytes on a rune boundary and reports
// whether anything was removed.
func TruncateText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// ReadableTime converts a middleware datetime ({"$date": <ms since epoch>}),
// a bare epoch number (seconds or milliseconds) or a numeric string into an
// RFC 3339 UTC string. Values that are not recognisable times, and times
// outside 2000-01-01 .. now+10y, are returned unchanged.
func ReadableTime(v interface{}) interface{} {
	raw := v
	if m, ok := v.(map[string]interface{}); ok {
		d, ok := m["$date"]
		if !ok || len(m) != 1 {
			return v
		}
		raw = d
	}
	var n float64
	switch c := raw.(type) {
	case float64:
		n = c
	case string:
		f, err := strconv.ParseFloat(c, 64)
		if err != nil {
			if t, err := time.Parse(time.RFC3339, c); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
			return v
		}
		n = f
	default:
		return v
	}
	var t time.Time
	if n > 1e11 { // milliseconds; seconds only reach 1e11 in the year 5138
		t = time.UnixMilli(int64(n))
	} else {
		t = time.Unix(int64(n), 0)
	}
	if t.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || t.After(time.Now().AddDate(10, 0, 0)) {
		return v
	}
	return t.UTC().Format(time.RFC3339)
}

// isAppEntry reports whether v looks like an app.query entry, the shape of
// an app.upgrade job result.
func isAppEntry(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, false
	}
	_, hasName := m["name"].(string)
	_, hasState := m["state"].(string)
	return m, hasName && hasState
}

// SummarizeApp reduces an app entry to the fields that matter after an
// upgrade: identity, state, versions and per-container state.
func SummarizeApp(app map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range []string{"name", "id", "state", "version", "human_version", "latest_version",
		"upgrade_available", "image_updates_available", "custom_app"} {
		if v, ok := app[k]; ok && v != nil {
			out[k] = v
		}
	}
	if aw, ok := app["active_workloads"].(map[string]interface{}); ok {
		details, _ := aw["container_details"].([]interface{})
		list := make([]interface{}, 0, len(details))
		for _, d := range details {
			c, ok := d.(map[string]interface{})
			if !ok {
				continue
			}
			if len(list) == maxContainers {
				break
			}
			list = append(list, map[string]interface{}{
				"service_name": c["service_name"], "image": c["image"], "state": c["state"],
			})
		}
		if len(details) > 0 {
			out["containers"] = list
			out["container_count"] = len(details)
		}
	}
	return out
}

// BoundResult returns a job result small enough to hand back to a caller.
// A result that serialises to at most MaxJobResultBytes is returned as is.
// A larger app entry is reduced to SummarizeApp, and anything else to a text
// preview. truncated reports that the returned value is not the full result;
// size is the serialised size of the original.
func BoundResult(v interface{}) (out interface{}, truncated bool, size int) {
	if v == nil {
		return nil, false, 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		s := fmt.Sprint(v)
		p, _ := TruncateText(s, MaxJobResultBytes)
		return p, true, len(s)
	}
	if len(b) <= MaxJobResultBytes {
		return v, false, len(b)
	}
	if app, ok := isAppEntry(v); ok {
		return SummarizeApp(app), true, len(b)
	}
	p, _ := TruncateText(string(b), MaxJobResultBytes)
	return p, true, len(b)
}

// SummarizeJob builds a bounded, readable view of a core.get_jobs entry:
// state, error, progress, times (RFC 3339 UTC) and, for an app result, the
// app name, state and versions. When the result was reduced, result_truncated
// is true and result_size_bytes gives the original size.
func SummarizeJob(job map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range []string{"id", "method", "state", "description"} {
		if v, ok := job[k]; ok && v != nil {
			out[k] = v
		}
	}
	if p, ok := job["progress"].(map[string]interface{}); ok {
		prog := map[string]interface{}{}
		if v, ok := p["percent"]; ok && v != nil {
			prog["percent"] = v
		}
		if s, ok := p["description"].(string); ok && s != "" {
			prog["description"], _ = TruncateText(s, MaxJobTextChars)
		}
		if len(prog) > 0 {
			out["progress"] = prog
		}
	}
	for _, k := range []string{"time_started", "time_finished"} {
		if v, ok := job[k]; ok && v != nil {
			out[k] = ReadableTime(v)
		}
	}
	if s, ok := job["error"].(string); ok && s != "" {
		var cut bool
		out["error"], cut = TruncateText(s, MaxJobTextChars)
		if cut {
			out["error_truncated"] = true
		}
	}
	if s, ok := job["exception"].(string); ok && s != "" {
		var cut bool
		out["exception"], cut = TruncateText(s, MaxJobTextChars)
		if cut {
			out["exception_truncated"] = true
		}
	}
	if r, ok := job["result"]; ok && r != nil {
		if app, ok := isAppEntry(r); ok {
			// An app entry carries the full config, ports, volumes and
			// networks; only identity, state and versions are kept.
			b, _ := json.Marshal(r)
			out["app"] = SummarizeApp(app)
			out["result_truncated"] = true
			out["result_size_bytes"] = len(b)
		} else {
			bounded, truncated, size := BoundResult(r)
			out["result"] = bounded
			out["result_truncated"] = truncated
			if truncated {
				out["result_size_bytes"] = size
			}
		}
	}
	return out
}
