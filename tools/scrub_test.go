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
	// t.Setenv does not retarget time.Local (Go reads TZ once at process
	// start). Assertions below pass explicit locations into calculateNextRun;
	// TZ is set only so accidental host-local use is less likely to match UTC CI.
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

func TestCalculateNextRunSpringForward(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2027-03-14 spring-forward: 02:00–02:59 do not exist. Daily 02:30 from
	// 03:15 EDT the same day must land on the next calendar day's 02:30, not
	// AddDate's gap-normalized 01:30.
	sched := map[string]interface{}{
		"minute": "30",
		"hour":   "2",
		"dom":    "*",
		"dow":    "*",
		"month":  "*",
	}
	from := time.Date(2027, 3, 14, 3, 15, 0, 0, ny)
	got := calculateNextRun(sched, from, ny)
	if got != "2027-03-15T02:30:00-04:00" {
		t.Fatalf("spring-forward daily next_run = %q, want 2027-03-15T02:30:00-04:00", got)
	}

	// Weekly Sunday 02:30 from the Friday before the spring-forward Sunday.
	weekly := map[string]interface{}{
		"minute": "30",
		"hour":   "2",
		"dom":    "*",
		"dow":    "0",
		"month":  "*",
	}
	fri := time.Date(2027, 3, 12, 10, 0, 0, 0, ny)
	gotWeekly := calculateNextRun(weekly, fri, ny)
	// Gap Sunday 02:30 is nonexistent; Go's Date normalizes that instant.
	if !strings.HasPrefix(gotWeekly, "2027-03-14T") {
		t.Fatalf("spring-forward weekly next_run = %q, want a 2027-03-14 timestamp", gotWeekly)
	}
	// After the gap (03:15 EDT), the next Sunday must keep schedule 02:30, not
	// AddDate's carried 01:30 from a gap-normalized wall clock.
	fromAfterGap := time.Date(2027, 3, 14, 3, 15, 0, 0, ny)
	gotNextWeek := calculateNextRun(weekly, fromAfterGap, ny)
	if gotNextWeek != "2027-03-21T02:30:00-04:00" {
		t.Fatalf("post-gap weekly next_run = %q, want 2027-03-21T02:30:00-04:00", gotNextWeek)
	}
}

func TestCalculateNextRunFallBackSanity(t *testing.T) {
	t.Setenv("TZ", "UTC")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	sched := map[string]interface{}{
		"minute": "30",
		"hour":   "1",
		"dom":    "*",
		"dow":    "*",
		"month":  "*",
	}
	// Fall-back Sunday 2026-11-01: 01:30 occurs twice. From 00:30 EDT, next is
	// the first 01:30 (EDT). From 03:00 EST, next is Monday 01:30 EST.
	before := time.Date(2026, 11, 1, 0, 30, 0, 0, ny)
	gotBefore := calculateNextRun(sched, before, ny)
	if gotBefore != "2026-11-01T01:30:00-04:00" {
		t.Fatalf("fall-back early next_run = %q, want 2026-11-01T01:30:00-04:00", gotBefore)
	}
	after := time.Date(2026, 11, 1, 3, 0, 0, 0, ny)
	gotAfter := calculateNextRun(sched, after, ny)
	if gotAfter != "2026-11-02T01:30:00-05:00" {
		t.Fatalf("fall-back late next_run = %q, want 2026-11-02T01:30:00-05:00", gotAfter)
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
	// eligible after (35-1)*24h = 2026-12-04 12:00 ET; first Sunday 00:00 is Dec 6.
	lastEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	from := time.Date(2026, 11, 9, 7, 30, 0, 0, ny)

	got, note, ok := estimateNextScrub(sched, from, ny, true, 35, &lastEnd, false)
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
	got, _, ok := estimateNextScrub(sched, from, utc, true, 35, &lastEnd, false)
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
	got, note, ok := estimateNextScrub(sched, from, time.UTC, true, 35, nil, false)
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
	_, _, ok := estimateNextScrub(sched, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), time.UTC, false, 35, &lastEnd, false)
	if ok {
		t.Fatal("disabled schedule must not produce an estimate")
	}
	out := map[string]interface{}{"next_run": "x"}
	attachNextScrubEstimate(out, sched, false, 35, time.UTC, time.Now(), &lastEnd, false)
	if _, exists := out["next_scrub_estimate"]; exists {
		t.Fatalf("disabled schedule leaked estimate fields: %v", out)
	}
	if _, exists := out["next_scrub_estimate_note"]; exists {
		t.Fatalf("disabled schedule leaked estimate note: %v", out)
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
	// threshold ≤ 1 → next cron tick (no (threshold-1) wait). From Oct 31
	// noon, next Sunday 00:00 is Nov 1 00:00 EDT (-04:00).
	lastEnd := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	from := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	got, _, ok := estimateNextScrub(sched, from, ny, true, 0, &lastEnd, false)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got != "2026-11-01T00:00:00-04:00" {
		t.Fatalf("DST estimate = %q, want 2026-11-01T00:00:00-04:00", got)
	}

	// After fall-back, threshold 0 from a Nov 2 end → next Sunday Nov 8 EST.
	lastEnd2 := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	from2 := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	got2, _, ok := estimateNextScrub(sched, from2, ny, true, 0, &lastEnd2, false)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got2 != "2026-11-08T00:00:00-05:00" {
		t.Fatalf("post-DST estimate = %q, want 2026-11-08T00:00:00-05:00", got2)
	}
}

func TestEstimateNextScrubDefaultWeeklyOffByOne(t *testing.T) {
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
	// Default weekly Sunday 00:00 / threshold 35. Scrub ended Sun 2026-10-04
	// 03:00 ET. Middleware is due after (35-1)*24h → Sat Nov 7 03:00 ET, so
	// the first Sunday tick is Nov 8. Adding full threshold calendar days
	// wrongly yields Nov 15 (week 6).
	lastEnd := time.Date(2026, 10, 4, 3, 0, 0, 0, ny)
	from := time.Date(2026, 10, 5, 12, 0, 0, 0, ny)
	got, _, ok := estimateNextScrub(sched, from, ny, true, 35, &lastEnd, false)
	if !ok {
		t.Fatal("expected estimate")
	}
	if got != "2026-11-08T00:00:00-05:00" {
		t.Fatalf("estimate = %q, want 2026-11-08T00:00:00-05:00 (not Nov 15)", got)
	}
	wrongFullThreshold := lastEnd.AddDate(0, 0, 35)
	wrongAnchor := from
	if wrongFullThreshold.After(wrongAnchor) {
		wrongAnchor = wrongFullThreshold
	}
	wrong := nextCronAtOrAfter(sched, wrongAnchor, ny)
	if wrong != "2026-11-15T00:00:00-05:00" {
		t.Fatalf("full-threshold control = %q, want Nov 15 (to prove the trap)", wrong)
	}
	if got == wrong {
		t.Fatal("estimate must use (threshold-1)*24h, not full threshold days")
	}
}

func TestEstimateNextScrubRunningSkipsNextTick(t *testing.T) {
	sched := map[string]interface{}{
		"minute": "0", "hour": "0", "dom": "*", "dow": "0", "month": "*",
	}
	lastEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, note, ok := estimateNextScrub(sched, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), time.UTC, true, 35, &lastEnd, true)
	if !ok {
		t.Fatal("expected running note")
	}
	if got != "" {
		t.Fatalf("running scrub must not return a timestamp, got %q", got)
	}
	if note != nextScrubEstimateNoteRunning {
		t.Fatalf("note = %q", note)
	}
	out := map[string]interface{}{}
	attachNextScrubEstimate(out, sched, true, 35, time.UTC, time.Now(), &lastEnd, true)
	if _, exists := out["next_scrub_estimate"]; exists {
		t.Fatalf("running scrub leaked estimate timestamp: %v", out)
	}
	if out["next_scrub_estimate_note"] != nextScrubEstimateNoteRunning {
		t.Fatalf("note field = %v", out["next_scrub_estimate_note"])
	}
}

func TestScrubScanRunning(t *testing.T) {
	running := map[string]interface{}{
		"scan": map[string]interface{}{"function": "SCRUB", "state": "SCANNING"},
	}
	if !scrubScanRunning(running) {
		t.Fatal("SCANNING scrub should count as running")
	}
	paused := map[string]interface{}{
		"scan": map[string]interface{}{
			"function": "SCRUB", "state": "SCANNING",
			"pause": map[string]interface{}{"$date": 1.0},
		},
	}
	if scrubScanRunning(paused) {
		t.Fatal("paused scrub should not count as running")
	}
	finished := map[string]interface{}{
		"scan": map[string]interface{}{"function": "SCRUB", "state": "FINISHED"},
	}
	if scrubScanRunning(finished) {
		t.Fatal("finished scrub should not count as running")
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
