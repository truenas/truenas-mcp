package tasks

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/internal/mwtime"
)

const (
	// MaxJobResultBytes bounds a single job summary returned to a caller: the
	// "job" object of tasks_get and each entry of query_jobs. It is measured
	// on the indented JSON as it is returned, in bytes (not characters).
	// Larger results are summarised (app entries) or cut to a preview, and
	// FitJobSummary shrinks the other fields until the whole summary fits.
	MaxJobResultBytes = 4096
	// MaxJobTextBytes bounds free-text job fields (error, exception) in bytes,
	// cut on a rune boundary. Within a job summary FitJobSummary may cut them
	// further so the whole summary stays within MaxJobResultBytes.
	MaxJobTextBytes = 2000
	// maxContainers bounds the per-container list in an app summary.
	maxContainers = 20
	// maxAppFieldBytes bounds each text field of an app summary.
	maxAppFieldBytes = 256
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

// readableTime renders a middleware datetime ({"$date": <ms since epoch>}),
// a bare epoch number or a numeric string as RFC 3339 UTC. Values that are
// not recognisable times, and times outside 2000-01-01 .. now+10y, are
// returned unchanged.
func readableTime(v interface{}) interface{} {
	if s, ts := mwtime.Normalize(v); ts > 0 {
		return s
	}
	return v
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
			out[k] = boundText(v, maxAppFieldBytes)
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
				"service_name": boundText(c["service_name"], maxAppFieldBytes),
				"image":        boundText(c["image"], maxAppFieldBytes),
				"state":        boundText(c["state"], maxAppFieldBytes),
			})
		}
		if len(details) > 0 {
			out["containers"] = list
			out["container_count"] = len(details)
		}
	}
	return out
}

// boundText cuts a string value to max bytes and returns other values as is.
func boundText(v interface{}, max int) interface{} {
	if s, ok := v.(string); ok {
		cut, _ := TruncateText(s, max)
		return cut
	}
	if b, err := json.Marshal(v); err == nil && len(b) > max {
		cut, _ := TruncateText(string(b), max)
		return cut
	}
	return v
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
// is true and result_size_bytes gives the original size. The whole summary
// fits MaxJobResultBytes (see FitJobSummary).
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
			prog["description"], _ = TruncateText(s, MaxJobTextBytes)
		}
		if len(prog) > 0 {
			out["progress"] = prog
		}
	}
	for _, k := range []string{"time_started", "time_finished"} {
		if v, ok := job[k]; ok && v != nil {
			out[k] = readableTime(v)
		}
	}
	if s, ok := job["error"].(string); ok && s != "" {
		var cut bool
		out["error"], cut = TruncateText(s, MaxJobTextBytes)
		if cut {
			out["error_truncated"] = true
		}
	}
	if s, ok := job["exception"].(string); ok && s != "" {
		var cut bool
		out["exception"], cut = TruncateText(s, MaxJobTextBytes)
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
	return FitJobSummary(out)
}

// summarySize is the size in bytes of a job summary as it appears in a tool
// response: indented JSON nested two levels deep (query_jobs entries; the
// "job" of tasks_get sits one level up, so it is slightly smaller).
func summarySize(m map[string]interface{}) int {
	b, err := json.MarshalIndent(m, "    ", "  ")
	if err != nil {
		return MaxJobResultBytes + 1
	}
	return len(b)
}

// FitJobSummary makes a job summary at most MaxJobResultBytes serialised, so
// the bound holds as a total and not per field. Free text (description,
// progress text, error, exception, a result preview) is cut progressively
// harder, noting each cut with a *_truncated flag; if that is still not
// enough, bulky optional fields are dropped and "summary_truncated" is set.
// m is modified and returned.
func FitJobSummary(m map[string]interface{}) map[string]interface{} {
	if summarySize(m) <= MaxJobResultBytes {
		return m
	}
	for _, limit := range []int{1024, 512, 256, 128} {
		shrinkJobText(m, limit)
		if summarySize(m) <= MaxJobResultBytes {
			return m
		}
	}
	m["summary_truncated"] = true
	for _, k := range []string{"app", "result", "exception", "error", "progress", "description"} {
		if _, ok := m[k]; ok {
			delete(m, k)
			if summarySize(m) <= MaxJobResultBytes {
				return m
			}
		}
	}
	// Only identity fields are left; bound them too.
	for _, k := range []string{"method", "state", "id", "time_started", "time_finished"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		m[k] = boundText(v, 64)
		if summarySize(m) <= MaxJobResultBytes {
			return m
		}
	}
	for k := range m {
		if k != "id" && k != "state" && k != "summary_truncated" {
			delete(m, k)
		}
	}
	return m
}

// shrinkJobText cuts every free-text field of a job summary to at most limit
// bytes.
func shrinkJobText(m map[string]interface{}, limit int) {
	if s, ok := m["description"].(string); ok {
		if cut, truncated := TruncateText(s, limit); truncated {
			m["description"] = cut
			m["description_truncated"] = true
		}
	}
	if p, ok := m["progress"].(map[string]interface{}); ok {
		if s, ok := p["description"].(string); ok {
			cut, truncated := TruncateText(s, limit)
			if truncated {
				np := map[string]interface{}{}
				for k, v := range p {
					np[k] = v
				}
				np["description"] = cut
				m["progress"] = np
			}
		}
	}
	for _, k := range []string{"error", "exception"} {
		if s, ok := m[k].(string); ok {
			if cut, truncated := TruncateText(s, limit); truncated {
				m[k] = cut
				m[k+"_truncated"] = true
			}
		}
	}
	if r, ok := m["result"]; ok {
		if s, isString := r.(string); isString {
			if cut, truncated := TruncateText(s, limit); truncated {
				m["result"] = cut
				m["result_truncated"] = true
			}
		} else if b, err := json.Marshal(r); err == nil && len(b) > limit {
			// A structured result becomes a text preview.
			cut, _ := TruncateText(string(b), limit)
			m["result"] = cut
			m["result_truncated"] = true
			if _, had := m["result_size_bytes"]; !had {
				m["result_size_bytes"] = len(b)
			}
		}
	}
	if app, ok := m["app"].(map[string]interface{}); ok && limit <= 256 {
		// Drop the per-container detail before anything else is lost.
		if _, has := app["containers"]; has {
			na := map[string]interface{}{}
			for k, v := range app {
				if k != "containers" {
					na[k] = v
				}
			}
			m["app"] = na
		}
	}
}
