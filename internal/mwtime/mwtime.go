// Package mwtime parses the time values TrueNAS middleware sends (a
// {"$date": <ms since epoch>} datetime, a bare epoch number or a string) and
// renders them as RFC 3339 UTC.
package mwtime

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// minTime is the earliest time accepted as a real date. TrueNAS boot
// environments and jobs can't predate it, so anything earlier (for example
// "20250105" read as epoch seconds, i.e. 1970) is a misparse.
var minTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// maxAhead is how far past now a time may be.
const maxAhead = 10 * 365 * 24 * time.Hour

// Normalize converts a middleware time (a boot environment "created" value,
// a job time_started or time_finished, ...) and returns it as an ISO-8601 UTC
// string plus Unix seconds. TrueNAS 25.04+ sends a datetime as {"$date": <ms since epoch>}; plain strings and numbers
// are accepted too. Numbers below 1e12 are seconds, larger ones
// milliseconds. Only times from 2000-01-01 to 10 years after now are
// accepted. Anything else (unparseable, out of range or overflowing) is
// returned raw with timestamp 0: a string as is, the value of $date if
// present, other values as compact JSON. A missing value yields ("", 0).
func Normalize(v interface{}) (string, int64) {
	raw := v
	if m, ok := v.(map[string]interface{}); ok {
		if d, ok := m["$date"]; ok {
			raw = d
		}
	}
	if raw == nil {
		return "", 0
	}
	t, ok := parseEpochOrRFC3339(raw)
	if !ok || !inRange(t) {
		return rawString(raw), 0
	}
	t = t.UTC()
	return t.Format(time.RFC3339), t.Unix()
}

// inRange reports whether t is a plausible middleware time.
func inRange(t time.Time) bool {
	return !t.Before(minTime) && !t.After(time.Now().Add(maxAhead))
}

// rawString renders an unparsed value for display: strings unchanged,
// anything else as compact JSON.
func rawString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// epochTime converts seconds (below 1e12) or milliseconds since the epoch.
// Values whose time falls outside the boot environment range, including
// ones that would overflow int64, are rejected before conversion.
func epochTime(n float64) (time.Time, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return time.Time{}, false
	}
	secs := n
	if math.Abs(n) >= 1e12 {
		secs = n / 1000
	}
	lo := float64(minTime.Unix())
	hi := float64(time.Now().Add(maxAhead).Unix())
	if secs < lo || secs > hi {
		return time.Time{}, false
	}
	if math.Abs(n) < 1e12 {
		return time.Unix(int64(n), 0), true
	}
	return time.UnixMilli(int64(n)), true
}

// parseEpochOrRFC3339 parses a number, a numeric string, an RFC 3339 string
// or a naive "2006-01-02T15:04:05" string (UTC, as the middleware uses
// utcfromtimestamp).
func parseEpochOrRFC3339(v interface{}) (time.Time, bool) {
	switch c := v.(type) {
	case float64:
		return epochTime(c)
	case string:
		if n, err := strconv.ParseFloat(c, 64); err == nil {
			return epochTime(n)
		}
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			return t, true
		}
		if t, err := time.Parse("2006-01-02T15:04:05", c); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
