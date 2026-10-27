package tools

import (
	"strings"
	"testing"
)

func TestValidateDatasetName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "valid dataset name",
			input:   "tank/shares/data",
			wantErr: false,
		},
		{
			name:    "valid with underscores",
			input:   "pool/my_dataset",
			wantErr: false,
		},
		{
			name:    "valid with hyphens",
			input:   "pool/my-dataset",
			wantErr: false,
		},
		{
			name:    "valid with dots",
			input:   "pool/my.dataset",
			wantErr: false,
		},
		{
			name:    "valid nested dataset",
			input:   "tank/shares/documents/2024",
			wantErr: false,
		},

		// Invalid cases - empty
		{
			name:    "empty name",
			input:   "",
			wantErr: true,
			errMsg:  "dataset name cannot be empty",
		},

		// Invalid cases - missing pool
		{
			name:    "missing pool separator",
			input:   "justpool",
			wantErr: true,
			errMsg:  "dataset name must include pool name (e.g., 'pool/dataset')",
		},

		// Invalid cases - leading/trailing slashes
		{
			name:    "leading slash",
			input:   "/tank/shares",
			wantErr: true,
			errMsg:  "dataset name cannot start or end with /",
		},
		{
			name:    "trailing slash",
			input:   "tank/shares/",
			wantErr: true,
			errMsg:  "dataset name cannot start or end with /",
		},

		// Invalid cases - consecutive slashes
		{
			name:    "consecutive slashes",
			input:   "tank//shares",
			wantErr: true,
			errMsg:  "dataset name cannot contain consecutive slashes",
		},

		// Invalid cases - invalid characters
		{
			name:    "space in name",
			input:   "tank/my dataset",
			wantErr: true,
			errMsg:  "dataset name contains invalid characters (only alphanumeric, /, _, ., - allowed)",
		},
		{
			name:    "special character @",
			input:   "tank/shares@snapshot",
			wantErr: true,
			errMsg:  "dataset name contains invalid characters (only alphanumeric, /, _, ., - allowed)",
		},
		{
			name:    "special character #",
			input:   "tank/shares#test",
			wantErr: true,
			errMsg:  "dataset name contains invalid characters (only alphanumeric, /, _, ., - allowed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDatasetName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateDatasetName() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateDatasetName() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateDatasetName() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestValidateEncryptionOptions(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]interface{}
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name: "valid with generate_key",
			input: map[string]interface{}{
				"generate_key": true,
			},
			wantErr: false,
		},
		{
			name: "valid with passphrase",
			input: map[string]interface{}{
				"passphrase": "mysecurepassword123",
			},
			wantErr: false,
		},
		{
			name: "valid with key",
			input: map[string]interface{}{
				"key": strings.Repeat("ab", 32),
			},
			wantErr: false,
		},
		{
			name: "valid with passphrase and pbkdf2iters",
			input: map[string]interface{}{
				"passphrase":  "mysecurepassword123",
				"pbkdf2iters": float64(1300000),
			},
			wantErr: false,
		},
		{
			name: "algorithm is not supported",
			input: map[string]interface{}{
				"passphrase": "mysecurepassword123",
				"algorithm":  "AES-256-GCM",
			},
			wantErr: true,
			errMsg:  "encryption_options.algorithm is not configurable through this tool; new encrypted datasets use AES-256-GCM",
		},

		// Invalid cases - both methods specified
		{
			name: "both generate_key and passphrase",
			input: map[string]interface{}{
				"generate_key": true,
				"passphrase":   "password123",
			},
			wantErr: true,
			errMsg:  "encryption_options accepts only one of generate_key=true, passphrase or key, not several",
		},

		// Invalid cases - no method specified
		{
			name:    "empty options",
			input:   map[string]interface{}{},
			wantErr: true,
			errMsg:  "encryption_options requires exactly one of generate_key=true, passphrase or key",
		},
		{
			name: "only algorithm specified",
			input: map[string]interface{}{
				"algorithm": "AES-256-GCM",
			},
			wantErr: true,
			errMsg:  "encryption_options.algorithm is not configurable through this tool; new encrypted datasets use AES-256-GCM",
		},

		// Invalid cases - passphrase too short
		{
			name: "passphrase too short",
			input: map[string]interface{}{
				"passphrase": "short",
			},
			wantErr: true,
			errMsg:  "passphrase must be at least 8 characters",
		},
		{
			name: "passphrase exactly 7 chars",
			input: map[string]interface{}{
				"passphrase": "1234567",
			},
			wantErr: true,
			errMsg:  "passphrase must be at least 8 characters",
		},

		// Edge cases
		{
			name: "passphrase exactly 8 chars",
			input: map[string]interface{}{
				"passphrase": "12345678",
			},
			wantErr: false,
		},
		{
			name: "generate_key false with passphrase",
			input: map[string]interface{}{
				"generate_key": false,
				"passphrase":   "mysecurepass",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEncryptionOptions(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateEncryptionOptions() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateEncryptionOptions() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateEncryptionOptions() unexpected error = %v", err)
				}
			}
		})
	}
}
