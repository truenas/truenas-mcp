package truenas

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestRoleSetUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantNil bool
		wantErr bool
	}{
		{
			name:  "list of strings",
			input: `{"roles": ["VM_WRITE", "FULL_ADMIN"]}`,
			want:  []string{"VM_WRITE", "FULL_ADMIN"},
		},
		{
			name:  "empty list",
			input: `{"roles": []}`,
			want:  []string{},
		},
		{
			name:  "set object",
			input: `{"roles": {"$set": ["FULL_ADMIN", "VM_WRITE"]}}`,
			want:  []string{"FULL_ADMIN", "VM_WRITE"},
		},
		{
			name:  "empty set object",
			input: `{"roles": {"$set": []}}`,
			want:  []string{},
		},
		{
			name:    "set object with null",
			input:   `{"roles": {"$set": null}}`,
			wantNil: true,
		},
		{
			name:    "null roles",
			input:   `{"roles": null}`,
			wantNil: true,
		},
		{
			name:    "missing roles",
			input:   `{}`,
			wantNil: true,
		},
		{
			name:    "invalid string",
			input:   `{"roles": "FULL_ADMIN"}`,
			wantErr: true,
		},
		{
			name:    "invalid number",
			input:   `{"roles": 42}`,
			wantErr: true,
		},
		{
			name:    "invalid boolean",
			input:   `{"roles": true}`,
			wantErr: true,
		},
		{
			name:    "invalid array element",
			input:   `{"roles": [123]}`,
			wantErr: true,
		},
		{
			name:    "invalid empty object",
			input:   `{"roles": {}}`,
			wantErr: true,
		},
		{
			name:    "invalid object wrong key",
			input:   `{"roles": {"other": ["FULL_ADMIN"]}}`,
			wantErr: true,
		},
		{
			name:    "invalid object extra key",
			input:   `{"roles": {"$set": ["FULL_ADMIN"], "extra": true}}`,
			wantErr: true,
		},
		{
			name:    "invalid set non-array string",
			input:   `{"roles": {"$set": "FULL_ADMIN"}}`,
			wantErr: true,
		},
		{
			name:    "invalid set non-array object",
			input:   `{"roles": {"$set": {}}}`,
			wantErr: true,
		},
		{
			name:    "invalid set element",
			input:   `{"roles": {"$set": [123]}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Roles RoleSet `json:"roles"`
			}
			err := json.Unmarshal([]byte(tc.input), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got roles=%v", tc.input, got.Roles)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.input, err)
			}
			if !slices.Equal(got.Roles, tc.want) {
				t.Errorf("roles = %v, want %v", got.Roles, tc.want)
			}
			if tc.wantNil && got.Roles != nil {
				t.Errorf("roles = %#v, want nil", got.Roles)
			}
			if !tc.wantNil && got.Roles == nil {
				t.Errorf("roles = nil, want non-nil slice %#v", tc.want)
			}
		})
	}

	t.Run("null clears existing value", func(t *testing.T) {
		r := RoleSet{"FULL_ADMIN"}
		if err := json.Unmarshal([]byte("null"), &r); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != nil {
			t.Errorf("roles = %#v, want nil", r)
		}
	})
}
