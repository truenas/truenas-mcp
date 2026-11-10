package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// parseJobID returns the TrueNAS job id from a job-method result: a JSON
// integer, a whole JSON number, or a non-empty array whose first element
// is the id. It returns an error when the id is missing or invalid,
// including 0, null, an empty array, or any other JSON value.
func parseJobID(result json.RawMessage) (int, error) {
	errInvalid := fmt.Errorf("missing or invalid job id")
	if len(bytes.TrimSpace(result)) == 0 || string(bytes.TrimSpace(result)) == "null" {
		return 0, errInvalid
	}
	var jobID int
	if err := json.Unmarshal(result, &jobID); err == nil {
		if jobID <= 0 {
			return 0, errInvalid
		}
		return jobID, nil
	}
	var f float64
	if err := json.Unmarshal(result, &f); err == nil {
		id, ok := wholePositiveInt(f)
		if !ok {
			return 0, errInvalid
		}
		return id, nil
	}
	var ints []int
	if err := json.Unmarshal(result, &ints); err == nil {
		if len(ints) == 0 || ints[0] <= 0 {
			return 0, errInvalid
		}
		return ints[0], nil
	}
	var floats []float64
	if err := json.Unmarshal(result, &floats); err == nil && len(floats) > 0 {
		id, ok := wholePositiveInt(floats[0])
		if !ok {
			return 0, errInvalid
		}
		return id, nil
	}
	return 0, errInvalid
}

func wholePositiveInt(f float64) (int, bool) {
	if f <= 0 || f > float64(math.MaxInt) || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}
