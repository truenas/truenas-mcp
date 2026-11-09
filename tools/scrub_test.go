package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatCronSchedule(t *testing.T) {
	tests := []struct {
		name     string
		schedule map[string]interface{}
		expected string
	}{
		{
			name: "weekly on Sunday",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "2",
				"dom":    "*",
				"dow":    "0",
				"month":  "*",
			},
			expected: "Weekly on Sunday at 2:0",
		},
		{
			name: "monthly on 1st",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "3",
				"dom":    "1",
				"dow":    "*",
				"month":  "*",
			},
			expected: "Monthly on 1st at 3:0",
		},
		{
			name: "daily at midnight",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "0",
				"dom":    "*",
				"dow":    "*",
				"month":  "*",
			},
			expected: "Daily at 0:0",
		},
		{
			name: "monthly on 23rd",
			schedule: map[string]interface{}{
				"minute": "30",
				"hour":   "14",
				"dom":    "23",
				"dow":    "*",
				"month":  "*",
			},
			expected: "Monthly on 23rd at 14:30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatCronSchedule(tt.schedule)
			if result != tt.expected {
				t.Errorf("formatCronSchedule() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestEstimateScrubDuration(t *testing.T) {
	tests := []struct {
		name      string
		sizeBytes int64
		minHours  int
		maxHours  int
	}{
		{
			name:      "1 TB pool",
			sizeBytes: 1099511627776, // 1 TiB
			minHours:  1,
			maxHours:  3,
		},
		{
			name:      "10 TB pool",
			sizeBytes: 10995116277760, // 10 TiB
			minHours:  5,
			maxHours:  7,
		},
		{
			name:      "small pool",
			sizeBytes: 10737418240, // 10 GiB
			minHours:  1,           // minimum is always 1
			maxHours:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := estimateScrubDuration(tt.sizeBytes)
			if result < tt.minHours || result > tt.maxHours {
				t.Errorf("estimateScrubDuration(%d) = %d hours, want between %d and %d",
					tt.sizeBytes, result, tt.minHours, tt.maxHours)
			}
		})
	}
}

func TestCalculateNextRun(t *testing.T) {
	// Use a fixed reference time for consistent testing
	referenceTime := time.Date(2026, 2, 9, 10, 0, 0, 0, time.UTC) // Monday, Feb 9, 2026 at 10:00

	tests := []struct {
		name     string
		schedule map[string]interface{}
		contains string // substring that should be in the result
	}{
		{
			name: "weekly on Sunday at 2am",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "2",
				"dom":    "*",
				"dow":    "0",
				"month":  "*",
			},
			contains: "2026-02-15", // Next Sunday (reference is Monday)
		},
		{
			name: "weekly on Monday (later today)",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "14",
				"dom":    "*",
				"dow":    "1",
				"month":  "*",
			},
			contains: "2026-02-09", // Later today (Monday 14:00; now is 10:00)
		},
		{
			name: "monthly on 15th",
			schedule: map[string]interface{}{
				"minute": "30",
				"hour":   "3",
				"dom":    "15",
				"dow":    "*",
				"month":  "*",
			},
			contains: "2026-02-15", // This month
		},
		{
			name: "daily at midnight",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "0",
				"dom":    "*",
				"dow":    "*",
				"month":  "*",
			},
			contains: "2026-02-10", // Tomorrow
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateNextRun(tt.schedule, referenceTime, time.UTC)
			if result == "" {
				t.Error("calculateNextRun() returned empty string")
			}
			if !strings.Contains(result, tt.contains) {
				t.Errorf("calculateNextRun() = %q, want substring %q", result, tt.contains)
			}
			// Just verify it returns a valid RFC3339 timestamp
			_, err := time.Parse(time.RFC3339, result)
			if err != nil {
				t.Errorf("calculateNextRun() returned invalid RFC3339 timestamp: %v", err)
			}
		})
	}
}

func TestMapKeys(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]bool
		expected []string
	}{
		{
			name: "sorted keys",
			input: map[string]bool{
				"zebra": true,
				"apple": true,
				"mango": true,
			},
			expected: []string{"apple", "mango", "zebra"},
		},
		{
			name:     "empty map",
			input:    map[string]bool{},
			expected: []string{},
		},
		{
			name: "single key",
			input: map[string]bool{
				"only": true,
			},
			expected: []string{"only"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapKeys(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("mapKeys() returned %d keys, want %d", len(result), len(tt.expected))
				return
			}
			for i, key := range result {
				if key != tt.expected[i] {
					t.Errorf("mapKeys()[%d] = %s, want %s", i, key, tt.expected[i])
				}
			}
		})
	}
}

func TestCalculateNextRunUsesExplicitLocation(t *testing.T) {
	// Force the process into Pacific so a regression that uses the host
	// zone instead of the passed location fails even on UTC CI runners.
	t.Setenv("TZ", "America/Los_Angeles")

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce the live bug: weekly Sunday 00:00, NAS in Eastern, MCP host in Pacific.
	// From Monday Nov 9 2026 07:30 PT, next Sunday midnight Eastern is 2026-11-15T00:00:00-05:00.
	from := time.Date(2026, 11, 9, 7, 30, 0, 0, la)
	sched := map[string]interface{}{
		"minute": "00",
		"hour":   "00",
		"dom":    "*",
		"dow":    "7",
		"month":  "*",
	}

	got := calculateNextRun(sched, from, ny)
	want := "2026-11-15T00:00:00-05:00"
	if got != want {
		t.Fatalf("NAS Eastern next_run = %q, want %q", got, want)
	}

	// Same wall-clock fields labelled with the host zone is the old bug.
	wrong := calculateNextRun(sched, from, la)
	if wrong != "2026-11-15T00:00:00-08:00" {
		t.Fatalf("Pacific control next_run = %q, want 2026-11-15T00:00:00-08:00", wrong)
	}
	if got == wrong {
		t.Fatal("next_run must differ when NAS and host zones differ")
	}
}

func TestCalculateNextRunDSTFallBack(t *testing.T) {
	t.Setenv("TZ", "UTC")

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	sched := map[string]interface{}{
		"minute": "00",
		"hour":   "00",
		"dom":    "*",
		"dow":    "0",
		"month":  "*",
	}

	// US fall-back Sunday is 2026-11-01. From the Saturday before, next
	// Sunday midnight is still on EDT (UTC-4); after the transition the
	// following week is EST (UTC-5).
	before := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	gotBefore := calculateNextRun(sched, before, ny)
	if gotBefore != "2026-11-01T00:00:00-04:00" {
		t.Fatalf("pre-fall-back next_run = %q, want 2026-11-01T00:00:00-04:00", gotBefore)
	}

	after := time.Date(2026, 11, 7, 12, 0, 0, 0, ny)
	gotAfter := calculateNextRun(sched, after, ny)
	if gotAfter != "2026-11-08T00:00:00-05:00" {
		t.Fatalf("post-fall-back next_run = %q, want 2026-11-08T00:00:00-05:00", gotAfter)
	}
}

func TestQueryScrubSchedulesUsesNASTimezone(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")

	sched := map[string]interface{}{
		"id":          35.0,
		"pool":        1.0,
		"pool_name":   "tank",
		"enabled":     true,
		"threshold":   35.0,
		"description": "",
		"schedule": map[string]interface{}{
			"minute": "00",
			"hour":   "00",
			"dom":    "*",
			"dow":    "7",
			"month":  "*",
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{
			"hostname": "nas.example.invalid",
			"timezone": "America/New_York",
		},
		"pool.scrub.query": []interface{}{sched},
		"pool.query": []interface{}{
			map[string]interface{}{"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE"},
		},
	}, nil)

	out, err := handleQueryScrubSchedules(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["next_run_timezone_note"]; ok {
		t.Fatalf("unexpected fallback note when timezone is available: %s", out)
	}
	list, _ := resp["scrub_schedules"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("schedules = %s", out)
	}
	entry := list[0].(map[string]interface{})
	nextRun, _ := entry["next_run"].(string)
	if _, err := time.Parse(time.RFC3339, nextRun); err != nil {
		t.Fatalf("next_run %q: %v", nextRun, err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	want := calculateNextRun(sched["schedule"].(map[string]interface{}), time.Now(), ny)
	if nextRun != want {
		t.Fatalf("next_run = %q, want %q (NAS Eastern)", nextRun, want)
	}
	hostLocal := calculateNextRun(sched["schedule"].(map[string]interface{}), time.Now(), la)
	if nextRun == hostLocal {
		t.Fatalf("next_run %q matches host-Pacific labelling; NAS zone was ignored", nextRun)
	}
}

func TestQueryScrubSchedulesTimezoneFallback(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")

	sched := map[string]interface{}{
		"id":          1.0,
		"pool":        1.0,
		"pool_name":   "tank",
		"enabled":     true,
		"threshold":   35.0,
		"description": "",
		"schedule": map[string]interface{}{
			"minute": "0",
			"hour":   "2",
			"dom":    "*",
			"dow":    "0",
			"month":  "*",
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{
		// Missing timezone field triggers the UTC fallback.
		"system.info": map[string]interface{}{
			"hostname": "nas.example.invalid",
		},
		"pool.scrub.query": []interface{}{sched},
		"pool.query": []interface{}{
			map[string]interface{}{"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE"},
		},
	}, nil)

	out, err := handleQueryScrubSchedules(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	note, _ := resp["next_run_timezone_note"].(string)
	if note != nextRunTimezoneNote {
		t.Fatalf("fallback note = %q, want %q; body=%s", note, nextRunTimezoneNote, out)
	}
	list, _ := resp["scrub_schedules"].([]interface{})
	entry := list[0].(map[string]interface{})
	nextRun, _ := entry["next_run"].(string)
	if !strings.HasSuffix(nextRun, "Z") && !strings.HasSuffix(nextRun, "+00:00") {
		t.Fatalf("fallback next_run %q should be UTC", nextRun)
	}
}

func TestNasTimezoneInvalidFallsBackToUTC(t *testing.T) {
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{
			"hostname": "nas.example.invalid",
			"timezone": "Not/A_Real_Zone",
		},
	}, nil)
	loc, name, assumed := nasTimezone(client)
	if !assumed || name != "UTC" || loc != time.UTC {
		t.Fatalf("nasTimezone() = %v %q assumed=%v, want UTC fallback", loc, name, assumed)
	}
}

func TestEstimateNextScrubThresholdSkip(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	sched := map[string]interface{}{
		"minute": "00",
		"hour":   "00",
		"dom":    "*",
		"dow":    "7",
		"month":  "*",
	}
	// Last finished scrub end: 2026-10-31 12:00 Eastern. Threshold 35 →
	// eligible 2026-12-05 12:00 ET (Saturday). First Sunday 00:00 tick is Dec 6.
	lastEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	from := time.Date(2026, 11, 9, 7, 30, 0, 0, ny)

	got, note, ok := estimateNextScrub(sched, from, ny, true, 35, &lastEnd)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got != "2026-12-06T00:00:00-05:00" {
		t.Fatalf("estimate = %q, want 2026-12-06T00:00:00-05:00", got)
	}
	if note != nextScrubEstimateNoteWithEnd {
		t.Fatalf("note = %q", note)
	}

	// next_run (next cron tick) would still be Nov 15 — estimate must differ.
	nextTick := calculateNextRun(sched, from, ny)
	if nextTick != "2026-11-15T00:00:00-05:00" {
		t.Fatalf("next tick control = %q, want 2026-11-15T00:00:00-05:00", nextTick)
	}
	if got == nextTick {
		t.Fatal("estimate must skip threshold window, not equal next_run")
	}
}

func TestEstimateNextScrubRunsAtNextTickWhenDue(t *testing.T) {
	t.Setenv("TZ", "UTC")
	utc := time.UTC
	sched := map[string]interface{}{
		"minute": "0",
		"hour":   "2",
		"dom":    "*",
		"dow":    "0",
		"month":  "*",
	}
	// Last end well past threshold relative to "now".
	lastEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, utc)
	from := time.Date(2026, 11, 9, 10, 0, 0, 0, utc) // Monday
	got, _, ok := estimateNextScrub(sched, from, utc, true, 35, &lastEnd)
	if !ok {
		t.Fatal("expected estimate")
	}
	want := calculateNextRun(sched, from, utc)
	if got != want {
		t.Fatalf("estimate = %q, want next tick %q", got, want)
	}
}

func TestEstimateNextScrubNoPriorScan(t *testing.T) {
	t.Setenv("TZ", "UTC")
	sched := map[string]interface{}{
		"minute": "0",
		"hour":   "0",
		"dom":    "*",
		"dow":    "0",
		"month":  "*",
	}
	from := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	got, note, ok := estimateNextScrub(sched, from, time.UTC, true, 35, nil)
	if !ok {
		t.Fatal("expected estimate")
	}
	if note != nextScrubEstimateNoteNoEnd {
		t.Fatalf("note = %q", note)
	}
	if got != calculateNextRun(sched, from, time.UTC) {
		t.Fatalf("estimate = %q, want next cron tick", got)
	}
}

func TestEstimateNextScrubDisabled(t *testing.T) {
	sched := map[string]interface{}{
		"minute": "0", "hour": "0", "dom": "*", "dow": "0", "month": "*",
	}
	lastEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _, ok := estimateNextScrub(sched, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), time.UTC, false, 35, &lastEnd)
	if ok {
		t.Fatal("disabled schedule must not produce an estimate")
	}
	out := map[string]interface{}{"next_run": "x"}
	attachNextScrubEstimate(out, sched, false, 35, time.UTC, time.Now(), &lastEnd)
	if _, exists := out["next_scrub_estimate"]; exists {
		t.Fatalf("disabled schedule leaked estimate fields: %v", out)
	}
}

func TestEstimateNextScrubDSTCrossing(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	sched := map[string]interface{}{
		"minute": "00",
		"hour":   "00",
		"dom":    "*",
		"dow":    "0",
		"month":  "*",
	}
	// Last end just before the 2026-11-01 fall-back; threshold 0 → eligible
	// at that end instant. First Sunday 00:00 at or after Oct 31 12:00 is
	// Nov 1 00:00 EDT (-04:00).
	lastEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	from := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	got, _, ok := estimateNextScrub(sched, from, ny, true, 0, &lastEnd)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got != "2026-11-01T00:00:00-04:00" {
		t.Fatalf("DST estimate = %q, want 2026-11-01T00:00:00-04:00", got)
	}

	// After fall-back, threshold still 0 from a Nov 2 end → next Sunday Nov 8 EST.
	lastEnd2 := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	from2 := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	got2, _, ok := estimateNextScrub(sched, from2, ny, true, 0, &lastEnd2)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got2 != "2026-11-08T00:00:00-05:00" {
		t.Fatalf("post-DST estimate = %q, want 2026-11-08T00:00:00-05:00", got2)
	}
}

func TestLastFinishedScrubEnd(t *testing.T) {
	ms := float64(time.Date(2026, 10, 31, 17, 0, 0, 0, time.UTC).UnixMilli())
	pool := map[string]interface{}{
		"scan": map[string]interface{}{
			"function": "SCRUB",
			"state":    "FINISHED",
			"end_time": map[string]interface{}{"$date": ms},
		},
	}
	got := lastFinishedScrubEnd(pool)
	if got == nil || !got.Equal(time.UnixMilli(int64(ms)).UTC()) {
		t.Fatalf("lastFinishedScrubEnd = %v", got)
	}
	// Non-SCRUB or non-FINISHED → nil
	pool["scan"].(map[string]interface{})["function"] = "RESILVER"
	if lastFinishedScrubEnd(pool) != nil {
		t.Fatal("resilver must not count")
	}
}

func TestQueryScrubSchedulesNextScrubEstimate(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	lastEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	sched := map[string]interface{}{
		"id": 35.0, "pool": 1.0, "pool_name": "tank", "enabled": true,
		"threshold": 35.0, "description": "",
		"schedule": map[string]interface{}{
			"minute": "00", "hour": "00", "dom": "*", "dow": "7", "month": "*",
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{
			"hostname": "nas.example.invalid", "timezone": "America/New_York",
		},
		"pool.scrub.query": []interface{}{sched},
		"pool.query": []interface{}{
			map[string]interface{}{
				"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE",
				"scan": map[string]interface{}{
					"function": "SCRUB", "state": "FINISHED",
					"end_time": map[string]interface{}{"$date": float64(lastEnd.UnixMilli())},
				},
			},
		},
	}, nil)

	out, err := handleQueryScrubSchedules(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	entry := resp["scrub_schedules"].([]interface{})[0].(map[string]interface{})
	if entry["next_scrub_estimate"] != "2026-12-06T00:00:00-05:00" {
		t.Fatalf("next_scrub_estimate = %v; body=%s", entry["next_scrub_estimate"], out)
	}
	if entry["next_scrub_estimate_note"] != nextScrubEstimateNoteWithEnd {
		t.Fatalf("note = %v", entry["next_scrub_estimate_note"])
	}
	// Disabled schedule omits estimate.
	sched["enabled"] = false
	client2, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{
			"hostname": "nas.example.invalid", "timezone": "America/New_York",
		},
		"pool.scrub.query": []interface{}{sched},
		"pool.query": []interface{}{
			map[string]interface{}{
				"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE",
				"scan": map[string]interface{}{
					"function": "SCRUB", "state": "FINISHED",
					"end_time": map[string]interface{}{"$date": float64(lastEnd.UnixMilli())},
				},
			},
		},
	}, nil)
	out2, err := handleQueryScrubSchedules(client2, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp2 map[string]interface{}
	if err := json.Unmarshal([]byte(out2), &resp2); err != nil {
		t.Fatal(err)
	}
	entry2 := resp2["scrub_schedules"].([]interface{})[0].(map[string]interface{})
	if _, ok := entry2["next_scrub_estimate"]; ok {
		t.Fatalf("disabled schedule should omit estimate: %s", out2)
	}
}
