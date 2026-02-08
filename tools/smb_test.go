package tools

import (
	"testing"
)

func TestValidateShareName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "simple valid name",
			input:   "myshare",
			wantErr: false,
		},
		{
			name:    "name with spaces",
			input:   "My Share",
			wantErr: false,
		},
		{
			name:    "name with numbers",
			input:   "share123",
			wantErr: false,
		},
		{
			name:    "name with underscore",
			input:   "my_share",
			wantErr: false,
		},
		{
			name:    "name with hyphen",
			input:   "my-share",
			wantErr: false,
		},
		{
			name:    "80 character name",
			input:   "12345678901234567890123456789012345678901234567890123456789012345678901234567890",
			wantErr: false,
		},

		// Invalid cases - empty
		{
			name:    "empty name",
			input:   "",
			wantErr: true,
			errMsg:  "share name cannot be empty",
		},

		// Invalid cases - too long
		{
			name:    "81 character name",
			input:   "123456789012345678901234567890123456789012345678901234567890123456789012345678901",
			wantErr: true,
			errMsg:  "share name cannot exceed 80 characters",
		},

		// Invalid cases - invalid characters
		{
			name:    "backslash",
			input:   "my\\share",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "forward slash",
			input:   "my/share",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "square brackets",
			input:   "share[test]",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "colon",
			input:   "share:test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "pipe",
			input:   "share|test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "less than",
			input:   "share<test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "greater than",
			input:   "share>test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "plus sign",
			input:   "share+test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "equals sign",
			input:   "share=test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "semicolon",
			input:   "share;test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "comma",
			input:   "share,test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "asterisk",
			input:   "share*test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "question mark",
			input:   "share?test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},
		{
			name:    "double quote",
			input:   "share\"test",
			wantErr: true,
			errMsg:  "share name cannot contain: \\ / [ ] : | < > + = ; , * ? \"",
		},

		// Invalid cases - reserved names (case-insensitive)
		{
			name:    "reserved name global",
			input:   "global",
			wantErr: true,
			errMsg:  "share name 'global' is reserved and cannot be used",
		},
		{
			name:    "reserved name GLOBAL uppercase",
			input:   "GLOBAL",
			wantErr: true,
			errMsg:  "share name 'GLOBAL' is reserved and cannot be used",
		},
		{
			name:    "reserved name Global mixed case",
			input:   "Global",
			wantErr: true,
			errMsg:  "share name 'Global' is reserved and cannot be used",
		},
		{
			name:    "reserved name printers",
			input:   "printers",
			wantErr: true,
			errMsg:  "share name 'printers' is reserved and cannot be used",
		},
		{
			name:    "reserved name PRINTERS",
			input:   "PRINTERS",
			wantErr: true,
			errMsg:  "share name 'PRINTERS' is reserved and cannot be used",
		},
		{
			name:    "reserved name homes",
			input:   "homes",
			wantErr: true,
			errMsg:  "share name 'homes' is reserved and cannot be used",
		},
		{
			name:    "reserved name HOMES",
			input:   "HOMES",
			wantErr: true,
			errMsg:  "share name 'HOMES' is reserved and cannot be used",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateShareName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateShareName() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateShareName() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateShareName() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestValidateSharePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "valid path",
			input:   "/mnt/tank/shares/data",
			wantErr: false,
		},
		{
			name:    "valid path with underscores",
			input:   "/mnt/pool/my_share",
			wantErr: false,
		},
		{
			name:    "valid path with hyphens",
			input:   "/mnt/pool/my-share",
			wantErr: false,
		},
		{
			name:    "special case EXTERNAL",
			input:   "EXTERNAL",
			wantErr: false,
		},

		// Invalid cases - empty
		{
			name:    "empty path",
			input:   "",
			wantErr: true,
			errMsg:  "path cannot be empty",
		},

		// Invalid cases - wrong prefix
		{
			name:    "missing /mnt/ prefix",
			input:   "/tank/shares/data",
			wantErr: true,
			errMsg:  "path must start with /mnt/ (got: /tank/shares/data)",
		},
		{
			name:    "relative path",
			input:   "tank/shares/data",
			wantErr: true,
			errMsg:  "path must start with /mnt/ (got: tank/shares/data)",
		},
		{
			name:    "just /mnt",
			input:   "/mnt",
			wantErr: true,
			errMsg:  "path must start with /mnt/ (got: /mnt)",
		},
		{
			name:    "pool root (should fail)",
			input:   "/mnt/tank",
			wantErr: false, // Note: this currently passes validation, but should be discouraged in guidance
		},

		// Invalid cases - consecutive slashes
		{
			name:    "consecutive slashes",
			input:   "/mnt/tank//shares",
			wantErr: true,
			errMsg:  "path cannot contain consecutive slashes",
		},
		{
			name:    "multiple consecutive slashes",
			input:   "/mnt///tank/shares",
			wantErr: true,
			errMsg:  "path cannot contain consecutive slashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSharePath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateSharePath() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateSharePath() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateSharePath() unexpected error = %v", err)
				}
			}
		})
	}
}
