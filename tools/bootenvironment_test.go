package tools

import (
	"testing"
	"time"
)

func TestSimplifyBootEnvironment(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]interface{}
		expected map[string]interface{}
	}{
		{
			name: "deletable boot environment",
			input: map[string]interface{}{
				"id":           "24.10-MASTER-20250105-010101",
				"created":      "2025-01-05T01:01:01Z",
				"used_bytes":   float64(1234567890),
				"active":       false,
				"activated":    false,
				"keep":         false,
				"can_activate": true,
			},
			expected: map[string]interface{}{
				"id":                "24.10-MASTER-20250105-010101",
				"created":           "2025-01-05T01:01:01Z",
				"created_timestamp": int64(0), // Will be calculated dynamically
				"size_bytes":        int64(1234567890),
				"size_human":        "1.15 GiB",
				"active":            false,
				"activated":         false,
				"protected":         false,
				"can_activate":      true,
				"deletable":         true,
				"deletion_blockers": []string{},
			},
		},
		{
			name: "active boot environment",
			input: map[string]interface{}{
				"id":           "24.10-MASTER-20250201-120000",
				"created":      "2025-02-01T12:00:00Z",
				"used_bytes":   float64(2000000000),
				"active":       true,
				"activated":    true,
				"keep":         false,
				"can_activate": false,
			},
			expected: map[string]interface{}{
				"id":                "24.10-MASTER-20250201-120000",
				"created":           "2025-02-01T12:00:00Z",
				"created_timestamp": int64(0), // Will be calculated dynamically
				"size_bytes":        int64(2000000000),
				"size_human":        "1.86 GiB",
				"active":            true,
				"activated":         true,
				"protected":         false,
				"can_activate":      false,
				"deletable":         false,
				"deletion_blockers": []string{"active", "activated"},
			},
		},
		{
			name: "protected boot environment",
			input: map[string]interface{}{
				"id":           "24.10-MASTER-20241201-000000",
				"created":      "2024-12-01T00:00:00Z",
				"used_bytes":   float64(1500000000),
				"active":       false,
				"activated":    false,
				"keep":         true,
				"can_activate": true,
			},
			expected: map[string]interface{}{
				"id":                "24.10-MASTER-20241201-000000",
				"created":           "2024-12-01T00:00:00Z",
				"created_timestamp": int64(0), // Will be calculated dynamically
				"size_bytes":        int64(1500000000),
				"size_human":        "1.40 GiB",
				"active":            false,
				"activated":         false,
				"protected":         true,
				"can_activate":      true,
				"deletable":         false,
				"deletion_blockers": []string{"protected"},
			},
		},
		{
			name: "activated but not active",
			input: map[string]interface{}{
				"id":           "24.10-MASTER-20250208-090000",
				"created":      "2025-02-08T09:00:00Z",
				"used_bytes":   float64(1800000000),
				"active":       false,
				"activated":    true,
				"keep":         false,
				"can_activate": true,
			},
			expected: map[string]interface{}{
				"id":                "24.10-MASTER-20250208-090000",
				"created":           "2025-02-08T09:00:00Z",
				"created_timestamp": int64(0), // Will be calculated dynamically
				"size_bytes":        int64(1800000000),
				"size_human":        "1.68 GiB",
				"active":            false,
				"activated":         true,
				"protected":         false,
				"can_activate":      true,
				"deletable":         false,
				"deletion_blockers": []string{"activated"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := simplifyBootEnvironment(tt.input)

			// Calculate expected timestamp from the created string
			expectedTimestamp := int64(0)
			if createdStr, ok := tt.input["created"].(string); ok && createdStr != "" {
				if t, err := time.Parse(time.RFC3339, createdStr); err == nil {
					expectedTimestamp = t.Unix()
				}
			}

			// Check all fields
			if result["id"] != tt.expected["id"] {
				t.Errorf("id mismatch: got %v, want %v", result["id"], tt.expected["id"])
			}
			if result["created"] != tt.expected["created"] {
				t.Errorf("created mismatch: got %v, want %v", result["created"], tt.expected["created"])
			}
			if result["created_timestamp"] != expectedTimestamp {
				t.Errorf("created_timestamp mismatch: got %v, want %v", result["created_timestamp"], expectedTimestamp)
			}
			if result["size_bytes"] != tt.expected["size_bytes"] {
				t.Errorf("size_bytes mismatch: got %v, want %v", result["size_bytes"], tt.expected["size_bytes"])
			}
			if result["size_human"] != tt.expected["size_human"] {
				t.Errorf("size_human mismatch: got %v, want %v", result["size_human"], tt.expected["size_human"])
			}
			if result["active"] != tt.expected["active"] {
				t.Errorf("active mismatch: got %v, want %v", result["active"], tt.expected["active"])
			}
			if result["activated"] != tt.expected["activated"] {
				t.Errorf("activated mismatch: got %v, want %v", result["activated"], tt.expected["activated"])
			}
			if result["protected"] != tt.expected["protected"] {
				t.Errorf("protected mismatch: got %v, want %v", result["protected"], tt.expected["protected"])
			}
			if result["can_activate"] != tt.expected["can_activate"] {
				t.Errorf("can_activate mismatch: got %v, want %v", result["can_activate"], tt.expected["can_activate"])
			}
			if result["deletable"] != tt.expected["deletable"] {
				t.Errorf("deletable mismatch: got %v, want %v", result["deletable"], tt.expected["deletable"])
			}

			// Check deletion_blockers
			resultBlockers := result["deletion_blockers"].([]string)
			expectedBlockers := tt.expected["deletion_blockers"].([]string)
			if len(resultBlockers) != len(expectedBlockers) {
				t.Errorf("deletion_blockers length mismatch: got %d, want %d", len(resultBlockers), len(expectedBlockers))
			}
			for i := range expectedBlockers {
				if i >= len(resultBlockers) || resultBlockers[i] != expectedBlockers[i] {
					t.Errorf("deletion_blockers[%d] mismatch: got %v, want %v", i, resultBlockers, expectedBlockers)
					break
				}
			}
		})
	}
}

func TestSortBootEnvironments(t *testing.T) {
	envs := []map[string]interface{}{
		{
			"id":                "env-c",
			"created_timestamp": int64(1000),
			"size_bytes":        int64(500),
		},
		{
			"id":                "env-a",
			"created_timestamp": int64(3000),
			"size_bytes":        int64(1500),
		},
		{
			"id":                "env-b",
			"created_timestamp": int64(2000),
			"size_bytes":        int64(1000),
		},
	}

	tests := []struct {
		name     string
		orderBy  string
		expected []string
	}{
		{
			name:     "sort by name",
			orderBy:  "name",
			expected: []string{"env-a", "env-b", "env-c"},
		},
		{
			name:     "sort by created (newest first)",
			orderBy:  "created",
			expected: []string{"env-a", "env-b", "env-c"},
		},
		{
			name:     "sort by size (largest first)",
			orderBy:  "size",
			expected: []string{"env-a", "env-b", "env-c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy of envs for each test
			testEnvs := make([]map[string]interface{}, len(envs))
			copy(testEnvs, envs)

			sortBootEnvironments(testEnvs, tt.orderBy)

			for i, expectedID := range tt.expected {
				if testEnvs[i]["id"] != expectedID {
					t.Errorf("sort order[%d] mismatch: got %v, want %v", i, testEnvs[i]["id"], expectedID)
				}
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{
			name:     "bytes",
			bytes:    500,
			expected: "500 B",
		},
		{
			name:     "kilobytes",
			bytes:    2048,
			expected: "2.00 KiB",
		},
		{
			name:     "megabytes",
			bytes:    5242880, // 5 MB
			expected: "5.00 MiB",
		},
		{
			name:     "gigabytes",
			bytes:    1234567890,
			expected: "1.15 GiB",
		},
		{
			name:     "terabytes",
			bytes:    2199023255552, // 2 TB
			expected: "2.00 TiB",
		},
		{
			name:     "zero bytes",
			bytes:    0,
			expected: "0 B",
		},
		{
			name:     "exactly 1 KiB",
			bytes:    1024,
			expected: "1.00 KiB",
		},
		{
			name:     "exactly 1 MiB",
			bytes:    1048576,
			expected: "1.00 MiB",
		},
		{
			name:     "exactly 1 GiB",
			bytes:    1073741824,
			expected: "1.00 GiB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBytes(tt.bytes)
			if result != tt.expected {
				t.Errorf("formatBytes(%d) = %v, want %v", tt.bytes, result, tt.expected)
			}
		})
	}
}

func TestDeletableComputation(t *testing.T) {
	tests := []struct {
		name      string
		active    bool
		activated bool
		keep      bool
		expected  bool
	}{
		{
			name:      "all false - deletable",
			active:    false,
			activated: false,
			keep:      false,
			expected:  true,
		},
		{
			name:      "active - not deletable",
			active:    true,
			activated: false,
			keep:      false,
			expected:  false,
		},
		{
			name:      "activated - not deletable",
			active:    false,
			activated: true,
			keep:      false,
			expected:  false,
		},
		{
			name:      "protected - not deletable",
			active:    false,
			activated: false,
			keep:      true,
			expected:  false,
		},
		{
			name:      "active and activated - not deletable",
			active:    true,
			activated: true,
			keep:      false,
			expected:  false,
		},
		{
			name:      "all true - not deletable",
			active:    true,
			activated: true,
			keep:      true,
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]interface{}{
				"id":           "test-env",
				"created":      time.Now().Format(time.RFC3339),
				"used_bytes":   float64(1000000000),
				"active":       tt.active,
				"activated":    tt.activated,
				"keep":         tt.keep,
				"can_activate": true,
			}

			result := simplifyBootEnvironment(env)
			deletable, ok := result["deletable"].(bool)
			if !ok {
				t.Fatal("deletable field is not a bool")
			}

			if deletable != tt.expected {
				t.Errorf("deletable = %v, want %v", deletable, tt.expected)
			}

			// Verify deletion_blockers
			blockers := result["deletion_blockers"].([]string)
			if tt.expected {
				// Should be deletable, so no blockers
				if len(blockers) != 0 {
					t.Errorf("expected no blockers for deletable environment, got %v", blockers)
				}
			} else {
				// Should not be deletable, so should have blockers
				if len(blockers) == 0 {
					t.Error("expected blockers for non-deletable environment, got none")
				}
			}
		})
	}
}
