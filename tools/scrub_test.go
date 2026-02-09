package tools

import (
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
	referenceTime := time.Date(2026, 2, 9, 10, 0, 0, 0, time.UTC) // Sunday, Feb 9, 2026 at 10:00

	tests := []struct {
		name     string
		schedule map[string]interface{}
		contains string // substring that should be in the result
	}{
		{
			name: "weekly on Sunday at 2am (already passed this week)",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "2",
				"dom":    "*",
				"dow":    "0",
				"month":  "*",
			},
			contains: "2026-02-16", // Next Sunday
		},
		{
			name: "weekly on Monday",
			schedule: map[string]interface{}{
				"minute": "0",
				"hour":   "14",
				"dom":    "*",
				"dow":    "1",
				"month":  "*",
			},
			contains: "2026-02-10", // Tomorrow (Monday)
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
			result := calculateNextRun(tt.schedule, referenceTime)
			if result == "" {
				t.Error("calculateNextRun() returned empty string")
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
