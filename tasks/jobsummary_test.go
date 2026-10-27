package tasks

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TruncateText must back off to a rune boundary. Every limit that falls in
// the middle of a multi-byte rune is exercised.
func TestTruncateTextBacksOffMidRune(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} { // 2, 3 and 4 bytes
		in := "a" + strings.Repeat(r, 10)
		for limit := 0; limit <= len(in)-1; limit++ {
			got, truncated := TruncateText(in, limit)
			if !truncated {
				t.Fatalf("%q limit %d: not reported as truncated", r, limit)
			}
			if !utf8.ValidString(got) {
				t.Errorf("%q limit %d: cut mid-rune: %q", r, limit, got)
			}
			if len(got) > limit {
				t.Errorf("%q limit %d: %d bytes", r, limit, len(got))
			}
			if !strings.HasPrefix(in, got) {
				t.Errorf("%q limit %d: not a prefix", r, limit)
			}
			// It must keep as much as fits: one rune more would not fit.
			if rest := in[len(got):]; len(got)+len(firstRune(rest)) <= limit {
				t.Errorf("%q limit %d: backed off too far (%d bytes kept)", r, limit, len(got))
			}
		}
	}
	// "a" + "é" cut at 2 bytes lands inside é: only "a" may remain.
	if got, _ := TruncateText("aéé", 2); got != "a" {
		t.Errorf("TruncateText(aéé, 2) = %q, want %q", got, "a")
	}
	if got, truncated := TruncateText("short", 100); got != "short" || truncated {
		t.Errorf("short text changed: %q %v", got, truncated)
	}
}

func firstRune(s string) string {
	_, n := utf8.DecodeRuneInString(s)
	return s[:n]
}

// jobSize is the size of a job summary as query_jobs returns it (indented
// two levels deep), the larger of the two places it is returned.
func jobSize(t *testing.T, m map[string]interface{}) int {
	t.Helper()
	b, err := json.MarshalIndent(m, "    ", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// The cap is a total per job summary, not per field: every free-text field at
// once, multi-byte text, and a huge result all stay within MaxJobResultBytes.
func TestSummarizeJobTotalWithinCap(t *testing.T) {
	long := strings.Repeat("€", 5000)
	cases := map[string]map[string]interface{}{
		"every text field": {
			"id": 7.0, "method": "app.upgrade", "state": "FAILED",
			"description": long,
			"progress":    map[string]interface{}{"percent": 50.0, "description": long},
			"error":       long, "exception": long,
			"result": strings.Repeat("r", 20000),
		},
		"structured result with long fields": {
			"id": 7.0, "method": "m", "state": "SUCCESS", "description": long, "error": long,
			"result": map[string]interface{}{"a": long, "b": long},
		},
		"app result with long names": {
			"id": 7.0, "method": "app.upgrade", "state": "SUCCESS", "description": long,
			"result": map[string]interface{}{
				"name": strings.Repeat("n", 5000), "id": "x", "state": strings.Repeat("S", 5000),
				"human_version": long, "version": long, "latest_version": long, "upgrade_available": true,
				"metadata":         map[string]interface{}{"title": long},
				"active_workloads": map[string]interface{}{"container_details": manyContainers(60)},
			},
		},
		"huge method and id": {
			"id": strings.Repeat("9", 9000), "method": long, "state": long, "description": long,
		},
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			out := SummarizeJob(job)
			if n := jobSize(t, out); n > MaxJobResultBytes {
				t.Errorf("summary is %d bytes, want <= %d: %v", n, MaxJobResultBytes, out)
			}
			b, _ := json.Marshal(out)
			if !utf8.Valid(b) {
				t.Error("summary is not valid UTF-8")
			}
		})
	}
}

func manyContainers(n int) []interface{} {
	var out []interface{}
	for i := 0; i < n; i++ {
		out = append(out, map[string]interface{}{
			"service_name": strings.Repeat("svc", 100), "image": strings.Repeat("img", 100), "state": "running",
		})
	}
	return out
}

// A modest job is not touched by the fitting.
func TestSummarizeJobSmallUnchanged(t *testing.T) {
	out := SummarizeJob(map[string]interface{}{
		"id": 7.0, "method": "app.upgrade", "state": "RUNNING", "description": "Upgrading",
		"progress": map[string]interface{}{"percent": 10.0, "description": "pulling"},
	})
	for _, k := range []string{"summary_truncated", "description_truncated", "error_truncated"} {
		if _, ok := out[k]; ok {
			t.Errorf("unexpected %s on a small job: %v", k, out)
		}
	}
	if out["description"] != "Upgrading" || out["progress"].(map[string]interface{})["description"] != "pulling" {
		t.Errorf("small job changed: %v", out)
	}
}

// Cut text is flagged so the caller knows it is partial.
func TestFitJobSummaryFlagsCutText(t *testing.T) {
	out := SummarizeJob(map[string]interface{}{
		"id": 1.0, "state": "FAILED",
		"description": strings.Repeat("d", 6000), "error": strings.Repeat("e", 6000), "exception": strings.Repeat("x", 6000),
	})
	if out["description_truncated"] != true || out["error_truncated"] != true || out["exception_truncated"] != true {
		t.Errorf("cuts not flagged: %v", out)
	}
	if jobSize(t, out) > MaxJobResultBytes {
		t.Errorf("too big: %d", jobSize(t, out))
	}
}

// The last-resort cut does not add identity keys the job did not have, and
// bounds non-string id/state values too.
func TestFitJobSummaryIdentityFallback(t *testing.T) {
	huge := []interface{}{}
	for i := 0; i < 2000; i++ {
		huge = append(huge, strings.Repeat("s", 4))
	}
	out := FitJobSummary(map[string]interface{}{"id": huge, "description": strings.Repeat("d", 6000)})
	for _, k := range []string{"method", "state", "time_started", "time_finished"} {
		if _, has := out[k]; has {
			t.Errorf("absent key %q added: %v", k, out[k])
		}
	}
	if jobSize(t, out) > MaxJobResultBytes {
		t.Errorf("too big: %d", jobSize(t, out))
	}
}
