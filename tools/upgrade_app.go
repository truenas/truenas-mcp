package tools

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

const (
	defaultUpgradeHealthTimeoutSeconds = 60
	maxUpgradeHealthTimeoutSeconds     = 600

	noChangelogText = "no changelog provided"
	// maxChangelogBytes bounds the changelog (release notes can be very long).
	maxChangelogBytes = 4000
	// maxUpgradeVersions bounds available_versions_for_upgrade.
	maxUpgradeVersions = 20
)

// upgradeHealthTimeout returns how long a finished upgrade job may wait for
// the app to report RUNNING: health_timeout_seconds, default 60s when absent.
// Zero means check once without waiting. The value must be an integer from 0
// to 600; anything else (a string, bool, null, fraction, negative or larger
// number) is an error rather than being coerced.
func upgradeHealthTimeout(args map[string]interface{}) (time.Duration, error) {
	v, present := args["health_timeout_seconds"]
	if !present {
		return defaultUpgradeHealthTimeoutSeconds * time.Second, nil
	}
	var secs float64
	switch n := v.(type) {
	case float64:
		secs = n
	case int:
		secs = float64(n)
	case int64:
		secs = float64(n)
	default:
		return 0, fmt.Errorf("health_timeout_seconds must be an integer from 0 to %d (got %v)", maxUpgradeHealthTimeoutSeconds, v)
	}
	if secs != math.Trunc(secs) || secs < 0 || secs > maxUpgradeHealthTimeoutSeconds {
		return 0, fmt.Errorf("health_timeout_seconds must be an integer from 0 to %d (got %v)", maxUpgradeHealthTimeoutSeconds, v)
	}
	return time.Duration(secs) * time.Second, nil
}

// snapshotNote explains what snapshot_requested does and does not tell the
// caller. Whatever snapshot_hostpaths is, middleware stops the app for the
// upgrade and snapshots the app's ix-volumes dataset for rollback;
// snapshot_hostpaths only requests snapshots of host paths, and of those only
// the ones that are ZFS datasets. app.upgrade never reports what it took.
func snapshotNote(requested bool) string {
	const always = " Independent of this option, TrueNAS stops the app during the upgrade and snapshots the app's ix-volumes dataset for rollback."
	if !requested {
		return "snapshot_hostpaths was false: no host-path snapshot is requested." + always
	}
	return "Host-path snapshots were requested. TrueNAS snapshots only host paths that are ZFS datasets and the upgrade job does not report which snapshots it took, so none is claimed." + always
}

// normalizeUpgradeSummary makes an app.upgrade_summary result safe to show:
// a null or blank changelog becomes "no changelog provided" instead of null,
// a very long changelog is truncated (changelog_truncated, changelog_length),
// a long version list is capped (available_versions_total) and middleware
// datetimes become RFC 3339 UTC strings. A list of summaries is normalized
// element by element; other shapes are returned unchanged.
func normalizeUpgradeSummary(summary interface{}) interface{} {
	switch s := summary.(type) {
	case []interface{}:
		out := make([]interface{}, len(s))
		for i, e := range s {
			out[i] = normalizeUpgradeSummary(e)
		}
		return out
	case map[string]interface{}:
		out := readableDatesMap(s)
		changelog, _ := out["changelog"].(string)
		if strings.TrimSpace(changelog) == "" {
			out["changelog"] = noChangelogText
		} else if cut, truncated := tasks.TruncateText(changelog, maxChangelogBytes); truncated {
			out["changelog"] = cut
			out["changelog_truncated"] = true
			out["changelog_length"] = len(changelog)
		}
		if versions, ok := out["available_versions_for_upgrade"].([]interface{}); ok && len(versions) > maxUpgradeVersions {
			out["available_versions_for_upgrade"] = versions[:maxUpgradeVersions]
			out["available_versions_truncated"] = true
			out["available_versions_total"] = len(versions)
		}
		return out
	}
	return summary
}
