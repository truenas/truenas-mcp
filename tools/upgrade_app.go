package tools

import (
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
// the app to report RUNNING: health_timeout_seconds, default 60s, capped at
// 600s. Zero means check once without waiting.
func upgradeHealthTimeout(args map[string]interface{}) time.Duration {
	secs := float64(defaultUpgradeHealthTimeoutSeconds)
	switch v := args["health_timeout_seconds"].(type) {
	case float64:
		secs = v
	case int:
		secs = float64(v)
	}
	if secs < 0 {
		secs = defaultUpgradeHealthTimeoutSeconds
	}
	if secs > maxUpgradeHealthTimeoutSeconds {
		secs = maxUpgradeHealthTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// snapshotNote explains what snapshot_requested does and does not tell the
// caller. app.upgrade only snapshots host paths that are ZFS datasets, skips
// ones that are not, and never reports which snapshots it took.
func snapshotNote(requested bool) string {
	if !requested {
		return "snapshot_hostpaths was not requested; no host-path snapshot is taken by this upgrade."
	}
	return "Host-path snapshots were requested. TrueNAS snapshots only host paths that are ZFS datasets and the upgrade job does not report which snapshots it took, so none is claimed."
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
