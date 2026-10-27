package tools

import (
	"fmt"
	"math"
	"strconv"
)

// readableDates returns v with every middleware datetime object
// ({"$date": <ms since epoch>}) replaced by an ISO-8601 UTC string, using
// the same parsing and 2000..now+10y range check as boot environment
// "created" times (parseBootEnvCreated). Out-of-range or unparseable values
// are passed through raw as parseBootEnvCreated does. Maps and slices are
// copied; other values are returned unchanged.
func readableDates(v interface{}) interface{} {
	switch c := v.(type) {
	case map[string]interface{}:
		if d, ok := c["$date"]; ok && len(c) == 1 && d != nil {
			s, _ := parseBootEnvCreated(c)
			return s
		}
		out := make(map[string]interface{}, len(c))
		for k, e := range c {
			out[k] = readableDates(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(c))
		for i, e := range c {
			out[i] = readableDates(e)
		}
		return out
	}
	return v
}

// readableDatesMap is readableDates for a JSON object.
func readableDatesMap(m map[string]interface{}) map[string]interface{} {
	out, _ := readableDates(m).(map[string]interface{})
	return out
}

// reportingUnits are the vertical_label values TrueNAS 27.0 middleware
// declares for its reporting graphs (plugins/reporting/netdata/graphs.py).
// They are used when reporting.graphs is not consulted or omits the label.
var reportingUnits = map[string]string{
	"cpu":       "%CPU",
	"cputemp":   "Celsius",
	"memory":    "Bytes",
	"load":      "Load",
	"uptime":    "Seconds",
	"disk":      "Kibibytes/s",
	"disktemp":  "Celsius",
	"interface": "Kilobits/s",
	"arcsize":   "bytes",
}

// legendNames returns a reporting graph's legend (["time", "a", "b", ...])
// as strings. Non-string entries become "".
func legendNames(metric map[string]interface{}) []string {
	raw, _ := metric["legend"].([]interface{})
	names := make([]string, len(raw))
	for i, l := range raw {
		names[i], _ = l.(string)
	}
	return names
}

// finiteValue returns v as a finite float64.
func finiteValue(v interface{}) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// isGapRow reports whether a reporting data row [ts, v1, v2, ...] carries
// no sample: every value is null, non-finite or 0. The middleware queries
// netdata with null2zero, so periods without data (common in MONTH ranges)
// arrive as all-zero rows.
func isGapRow(row []interface{}) bool {
	if len(row) < 2 {
		return true
	}
	for _, v := range row[1:] {
		if f, ok := finiteValue(v); ok && f != 0 {
			return false
		}
	}
	return true
}

// columnPoints returns the finite values of data column col (1 = first
// series). With skipGaps, all-zero/null rows (see isGapRow) are skipped.
func columnPoints(metric map[string]interface{}, col int, skipGaps bool) []float64 {
	rows, _ := metric["data"].([]interface{})
	points := make([]float64, 0, len(rows))
	for _, r := range rows {
		row, ok := r.([]interface{})
		if !ok || len(row) <= col {
			continue
		}
		if skipGaps && isGapRow(row) {
			continue
		}
		if f, ok := finiteValue(row[col]); ok {
			points = append(points, f)
		}
	}
	return points
}

// gapFreeRows splits reporting data rows into those with samples and the
// number of gap rows.
func gapFreeRows(rows []interface{}) (valid []interface{}, gaps int) {
	valid = make([]interface{}, 0, len(rows))
	for _, r := range rows {
		row, ok := r.([]interface{})
		if !ok || isGapRow(row) {
			gaps++
			continue
		}
		valid = append(valid, r)
	}
	return valid, gaps
}

// aggregateRows computes min/mean/max per legend series over rows, in the
// middleware's aggregations shape: {"min": {series: v}, "mean": ..., "max": ...}.
// Series with no finite values are omitted.
func aggregateRows(legend []string, rows []interface{}) map[string]interface{} {
	agg := map[string]interface{}{}
	mins, means, maxs := map[string]interface{}{}, map[string]interface{}{}, map[string]interface{}{}
	for col := 1; col < len(legend); col++ {
		lo, hi, sum, n := math.Inf(1), math.Inf(-1), 0.0, 0
		for _, r := range rows {
			row, _ := r.([]interface{})
			if len(row) <= col {
				continue
			}
			if f, ok := finiteValue(row[col]); ok {
				lo, hi, sum, n = math.Min(lo, f), math.Max(hi, f), sum+f, n+1
			}
		}
		if n == 0 {
			continue
		}
		mins[legend[col]], means[legend[col]], maxs[legend[col]] = lo, sum/float64(n), hi
	}
	agg["min"], agg["mean"], agg["max"] = mins, means, maxs
	return agg
}

// sampleRows returns the first 10 and, when there are more than 20 rows,
// the last 10 rows.
func sampleRows(rows []interface{}) []interface{} {
	sample := make([]interface{}, 0, 20)
	for i := 0; i < 10 && i < len(rows); i++ {
		sample = append(sample, rows[i])
	}
	if len(rows) > 20 {
		sample = append(sample, rows[len(rows)-10:]...)
	}
	return sample
}

// lastSampleAfterEnd returns how many seconds the newest row's timestamp is
// past the graph's "end", or 0. netdata labels each averaged bucket with
// its aligned end time, so for long ranges (MONTH buckets are ~15 minutes)
// the newest bucket can be stamped slightly after the requested end.
func lastSampleAfterEnd(graph map[string]interface{}, rows []interface{}) float64 {
	end, ok := finiteValue(graph["end"])
	if !ok || len(rows) == 0 {
		return 0
	}
	row, _ := rows[len(rows)-1].([]interface{})
	if len(row) == 0 {
		return 0
	}
	if ts, ok := finiteValue(row[0]); ok && ts > end {
		return ts - end
	}
	return 0
}

// summarizeSystemGraph condenses one get_system_metrics graph. Metadata
// (name, identifier, legend, start, end, ...) is kept. Rows with no sample
// (all zero/null, the middleware's null2zero gap filling) are dropped from
// the data sample; when there are any, "aggregations" is recomputed over
// the remaining rows and the middleware's values, which include the gaps,
// are kept as "aggregations_reported". Graphs in skipZeroAggregationGraphs
// keep the middleware's aggregations, which already skip zero values.
func summarizeSystemGraph(graph map[string]interface{}, name string) map[string]interface{} {
	summary := make(map[string]interface{}, len(graph)+4)
	for k, v := range graph {
		if k != "data" {
			summary[k] = v
		}
	}
	if unit, ok := reportingUnits[name]; ok {
		summary["unit"] = unit
	}
	rows, ok := graph["data"].([]interface{})
	if !ok {
		return summary
	}
	valid, gaps := gapFreeRows(rows)
	summary["data_points_total"] = len(rows)
	summary["data_points_valid"] = len(valid)
	summary["data_sample"] = sampleRows(valid)
	if gaps > 0 && skipZeroAggregationGraphs[name] {
		// The middleware already skipped zero values when aggregating, per
		// value rather than per row, so its aggregations are kept.
		summary["gap_samples_excluded"] = gaps
		summary["gap_note"] = "Rows whose values are all 0 or null are periods without data (zero-filled by TrueNAS) and are excluded from data_sample. aggregations are as reported by TrueNAS, which already skips zero values for this graph."
	} else if gaps > 0 {
		summary["gap_samples_excluded"] = gaps
		if reported, ok := graph["aggregations"]; ok {
			summary["aggregations_reported"] = reported
		}
		summary["aggregations"] = aggregateRows(legendNames(graph), valid)
		summary["gap_note"] = "Rows whose values are all 0 or null are periods without data (zero-filled by TrueNAS) and are excluded from data_sample and aggregations."
	}
	if after := lastSampleAfterEnd(graph, valid); after > 0 {
		summary["last_sample_after_end_seconds"] = after
	}
	return summary
}

// snapshotProperties are the ZFS properties query_snapshots requests.
var snapshotProperties = []interface{}{"creation", "used", "referenced"}

// snapshotProperty returns a snapshot property's parsed value, falling back
// to rawvalue. Properties are {value, rawvalue, source, parsed}.
func snapshotProperty(props map[string]interface{}, name string) (interface{}, bool) {
	p, ok := props[name].(map[string]interface{})
	if !ok {
		return nil, false
	}
	if v, ok := p["parsed"]; ok && v != nil {
		return v, true
	}
	if v, ok := p["rawvalue"]; ok && v != nil && v != "" {
		return v, true
	}
	return nil, false
}

// snapshotCreated returns the "creation" property as an ISO-8601 UTC string
// and Unix seconds. It accepts epoch seconds (27.0), {"$date": ms} (older
// releases) or numeric strings; anything unparseable yields ts 0.
func snapshotCreated(props map[string]interface{}) (string, int64) {
	for _, get := range []string{"parsed", "rawvalue"} {
		p, ok := props["creation"].(map[string]interface{})
		if !ok {
			return "", 0
		}
		if v, ok := p[get]; ok && v != nil {
			if s, ts := parseBootEnvCreated(v); ts > 0 {
				return s, ts
			}
		}
	}
	return "", 0
}

// snapshotPropertyBytes returns a size property in bytes.
func snapshotPropertyBytes(props map[string]interface{}, name string) (int64, bool) {
	v, ok := snapshotProperty(props, name)
	if !ok {
		return 0, false
	}
	var f float64
	switch c := v.(type) {
	case float64:
		f = c
	case string:
		n, err := strconv.ParseFloat(c, 64)
		if err != nil {
			return 0, false
		}
		f = n
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > math.MaxInt64/2 {
		return 0, false
	}
	return int64(f), true
}

// skipZeroAggregationGraphs are the reporting graphs whose middleware
// aggregations already skip zero values (skip_zero_values_in_aggregation in
// the 27.0 netdata graph classes). Recomputing them here would count zeros
// the middleware skipped, so their aggregations are kept as reported.
var skipZeroAggregationGraphs = map[string]bool{
	"cputemp":  true,
	"disktemp": true,
}

// legendColumn returns the data column of the legend series called name,
// or fallback when the legend has no such series.
func legendColumn(metric map[string]interface{}, name string, fallback int) int {
	for i, l := range legendNames(metric) {
		if i > 0 && l == name {
			return i
		}
	}
	return fallback
}

// timedSample is one finite value of a reporting series with its row
// timestamp (0 when the row has none).
type timedSample struct {
	ts float64
	v  float64
}

// columnSamples is columnPoints with timestamps. keep, when non-nil, drops
// values it returns false for.
func columnSamples(metric map[string]interface{}, col int, skipGaps bool, keep func(float64) bool) []timedSample {
	rows, _ := metric["data"].([]interface{})
	samples := make([]timedSample, 0, len(rows))
	for _, r := range rows {
		row, ok := r.([]interface{})
		if !ok || len(row) <= col {
			continue
		}
		if skipGaps && isGapRow(row) {
			continue
		}
		f, ok := finiteValue(row[col])
		if !ok || (keep != nil && !keep(f)) {
			continue
		}
		ts, _ := finiteValue(row[0])
		samples = append(samples, timedSample{ts: ts, v: f})
	}
	return samples
}

// sampleValues returns the values of samples.
func sampleValues(samples []timedSample) []float64 {
	vals := make([]float64, len(samples))
	for i, s := range samples {
		vals[i] = s.v
	}
	return vals
}

// completeSamples drops trailing samples stamped after the graph's "end".
// Netdata stamps aligned buckets by their end, so with MONTH and other
// long ranges the last bucket can lie past "end" and cover only part of its
// interval; it must not feed "current" figures. It returns the remaining
// samples and a note for the output: partialSampleNote when samples were
// dropped, allPartialSampleNote when every sample lies past "end" (they are
// then returned unchanged, as there is nothing else to use), or "". A
// graph without "end" returns samples unchanged with no note.
func completeSamples(graph map[string]interface{}, samples []timedSample) ([]timedSample, string) {
	end, ok := finiteValue(graph["end"])
	if !ok || end <= 0 {
		return samples, ""
	}
	n := len(samples)
	for n > 0 && samples[n-1].ts > end {
		n--
	}
	switch {
	case n == len(samples):
		return samples, ""
	case n == 0:
		return samples, allPartialSampleNote
	}
	return samples[:n], partialSampleNote
}

// partialSampleNote explains a dropped partial last bucket.
const partialSampleNote = "The last sample is stamped after the graph's end (a partial bucket) and is not used for current figures; it is still included in average, peak and trend."

// allPartialSampleNote is given when every sample is past the graph's end.
const allPartialSampleNote = "Every sample is stamped after the graph's end (a partial bucket), so the current figures come from a partial bucket."

// atLeastOne returns v as an int, or 1 when v is missing or below 1 (the
// VM schema's default and minimum for vcpus, cores and threads).
func atLeastOne(v float64) int {
	if v < 1 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	return int(v)
}

// formatGiB formats a size in MiB as GiB: one decimal from 1 GiB up, two
// below, so small VMs (the minimum is 20 MiB) don't show as "0.0 GiB".
func formatGiB(mib float64) string {
	if mib < 1024 {
		return fmt.Sprintf("%.2f GiB", mib/1024.0)
	}
	return fmt.Sprintf("%.1f GiB", mib/1024.0)
}
