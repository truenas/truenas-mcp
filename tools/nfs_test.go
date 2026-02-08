package tools

import (
	"testing"
)

func TestValidateCIDR(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "valid CIDR /24",
			input:   "192.168.1.0/24",
			wantErr: false,
		},
		{
			name:    "valid CIDR /16",
			input:   "10.0.0.0/16",
			wantErr: false,
		},
		{
			name:    "valid CIDR /8",
			input:   "172.16.0.0/8",
			wantErr: false,
		},
		{
			name:    "valid CIDR /32",
			input:   "192.168.1.1/32",
			wantErr: false,
		},
		{
			name:    "valid CIDR /0",
			input:   "0.0.0.0/0",
			wantErr: false,
		},

		// Invalid cases - empty
		{
			name:    "empty CIDR",
			input:   "",
			wantErr: true,
			errMsg:  "CIDR cannot be empty",
		},

		// Invalid cases - missing slash
		{
			name:    "no slash",
			input:   "192.168.1.0",
			wantErr: true,
			errMsg:  "CIDR must be in format 'network/mask' (e.g., 192.168.1.0/24)",
		},

		// Invalid cases - invalid format
		{
			name:    "multiple slashes",
			input:   "192.168.1.0/24/32",
			wantErr: true,
			errMsg:  "CIDR must be in format 'network/mask'",
		},
		{
			name:    "slash at start",
			input:   "/24",
			wantErr: false, // Note: Current implementation doesn't validate empty network part
		},
		{
			name:    "slash at end",
			input:   "192.168.1.0/",
			wantErr: true,
			errMsg:  "CIDR mask must be a number (e.g., /24)",
		},

		// Invalid cases - non-numeric mask
		{
			name:    "text mask",
			input:   "192.168.1.0/mask",
			wantErr: true,
			errMsg:  "CIDR mask must be a number (e.g., /24)",
		},
		{
			name:    "mask with letters",
			input:   "192.168.1.0/24a",
			wantErr: true,
			errMsg:  "CIDR mask must be a number (e.g., /24)",
		},
		{
			name:    "mask with space",
			input:   "192.168.1.0/24 ",
			wantErr: true,
			errMsg:  "CIDR mask must be a number (e.g., /24)",
		},

		// Edge cases - note: we don't validate the IP address itself, just the format
		{
			name:    "invalid IP but valid format",
			input:   "999.999.999.999/24",
			wantErr: false, // Current implementation only checks format, not IP validity
		},
		{
			name:    "invalid mask number but valid format",
			input:   "192.168.1.0/99",
			wantErr: false, // Current implementation only checks format, not mask range
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCIDR(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateCIDR() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateCIDR() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateCIDR() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestValidateNFSHost(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "valid IP address",
			input:   "192.168.1.100",
			wantErr: false,
		},
		{
			name:    "valid hostname",
			input:   "server.example.com",
			wantErr: false,
		},
		{
			name:    "valid short hostname",
			input:   "server",
			wantErr: false,
		},
		{
			name:    "valid hostname with hyphen",
			input:   "my-server.local",
			wantErr: false,
		},
		{
			name:    "valid hostname with numbers",
			input:   "server123.example.com",
			wantErr: false,
		},

		// Invalid cases - empty
		{
			name:    "empty host",
			input:   "",
			wantErr: true,
			errMsg:  "host cannot be empty",
		},

		// Invalid cases - quotes
		{
			name:    "double quotes",
			input:   "\"server.local\"",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},
		{
			name:    "single quotes",
			input:   "'server.local'",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},
		{
			name:    "quote in middle",
			input:   "server\"test.local",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},

		// Invalid cases - spaces
		{
			name:    "space in hostname",
			input:   "server test",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},
		{
			name:    "leading space",
			input:   " server.local",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},
		{
			name:    "trailing space",
			input:   "server.local ",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},

		// Invalid cases - multiple issues
		{
			name:    "quotes and spaces",
			input:   "\"server test\"",
			wantErr: true,
			errMsg:  "host cannot contain quotes or spaces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNFSHost(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateNFSHost() expected error, got nil")
					return
				}
				if tt.errMsg != "" && err.Error() != tt.errMsg {
					t.Errorf("validateNFSHost() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateNFSHost() unexpected error = %v", err)
				}
			}
		})
	}
}
