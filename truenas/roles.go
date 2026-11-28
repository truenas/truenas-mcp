package truenas

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// RoleSet decodes a TrueNAS middleware role set. Middleware sends privilege
// roles as a $set object ({"$set": [...]}); a plain JSON array of strings is
// also accepted, and null decodes to nil.
type RoleSet []string

// UnmarshalJSON decodes a TrueNAS middleware set object ({"$set": [...]}) or a
// JSON array of strings into r.
func (r *RoleSet) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*r = nil
		return nil
	}
	switch trimmed[0] {
	case '[':
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return fmt.Errorf("invalid roles array: %w", err)
		}
		*r = list
		return nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return fmt.Errorf("invalid roles object: %w", err)
		}
		rawSet, ok := obj["$set"]
		if !ok || len(obj) != 1 {
			return fmt.Errorf(`roles object must contain only a "$set" key`)
		}
		var list []string
		if err := json.Unmarshal(rawSet, &list); err != nil {
			return fmt.Errorf(`invalid roles "$set" value: %w`, err)
		}
		*r = list
		return nil
	default:
		return fmt.Errorf(`roles must be a JSON array of strings or {"$set": [...]}`)
	}
}
