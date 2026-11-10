package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

const (
	defaultUpgradeHealthTimeoutSeconds = 60
	maxUpgradeHealthTimeoutSeconds     = 600

	// minUpgradeJobSeconds and maxUpgradeJobSeconds bound the app.upgrade
	// job itself (stop, snapshot, image pull, redeploy), before the health
	// check that follows it.
	minUpgradeJobSeconds = 30
	maxUpgradeJobSeconds = 300

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

// upgradeEstimatedTime is the upgrade_app time estimate: the upgrade job
// plus, at most, the health check wait of healthTimeout after it.
func upgradeEstimatedTime(healthTimeout time.Duration) *EstimatedTime {
	health := int(healthTimeout / time.Second)
	return &EstimatedTime{
		MinSeconds: minUpgradeJobSeconds,
		MaxSeconds: maxUpgradeJobSeconds + health,
		Note: fmt.Sprintf("Time varies based on image size and network speed: up to %ds for the upgrade job, plus up to %ds waiting for the app to report RUNNING (health_timeout_seconds)",
			maxUpgradeJobSeconds, health),
	}
}

// snapshotNote explains what snapshot_requested does and does not tell the
// caller. Whatever snapshot_hostpaths is, for a version upgrade middleware
// stops the app and snapshots its ix-volumes dataset (if it has one);
// snapshot_hostpaths only requests snapshots of host paths, and of those only
// the ones that are ZFS datasets. Custom apps and image-only updates take no
// snapshot at all. app.upgrade never reports what it took.
func snapshotNote(requested bool) string {
	const always = " Independent of this option, for a version upgrade TrueNAS stops the app and snapshots its ix-volumes dataset (if it has one) for rollback; custom apps and image-only updates are instead redeployed with updated images, without a stop or any snapshot."
	if !requested {
		return "snapshot_hostpaths was false: no host-path snapshot is requested." + always
	}
	return "Host-path snapshots were requested. TrueNAS snapshots only host paths that are ZFS datasets and the upgrade job does not report which snapshots it took, so none is claimed." + always
}

// upgradeSnapshotStep describes the snapshot step of the upgrade plan. It
// names what is snapshotted, never a snapshot name: app.upgrade does not
// report which snapshots it took.
func upgradeSnapshotStep(hostpaths bool) string {
	if hostpaths {
		return "Snapshot the app's ix-volumes dataset (if it has one) and its host paths (those that are ZFS datasets) before the upgrade, for rollback"
	}
	return "Snapshot the app's ix-volumes dataset (if it has one) before the upgrade, for rollback (host paths are not snapshotted: snapshot_hostpaths is false)"
}

// upgradeIsImageOnly reports whether app.upgrade will only pull updated
// images and redeploy the app. Middleware takes that path for custom apps,
// and for catalog apps whose only update is new images; it then does not
// stop the app, takes no snapshot and ignores snapshot_hostpaths. On that
// path app.upgrade_summary names the app's current version as the target,
// which is how an image-only catalog update is recognised here.
func upgradeIsImageOnly(app map[string]interface{}, summary interface{}) (bool, string) {
	if custom, _ := app["custom_app"].(bool); custom {
		return true, "this is a custom app"
	}
	s, _ := summary.(map[string]interface{})
	if l, ok := summary.([]interface{}); ok && len(l) > 0 {
		s, _ = l[0].(map[string]interface{})
	}
	target, _ := s["upgrade_version"].(string)
	current, _ := app["version"].(string)
	if target = strings.TrimSpace(target); target != "" && target == strings.TrimSpace(current) {
		return true, "this is an image-only update with no new app version"
	}
	return false, ""
}

// upgradeTargetDescription names the version an upgrade will install. An
// explicit version is used as given; for "latest" the version is resolved
// from the app.upgrade_summary result (upgrade_* first, then latest_*), and
// only if the summary names none is "the latest available version" used.
func upgradeTargetDescription(version string, summary interface{}) string {
	if version != "latest" {
		return version
	}
	s, _ := summary.(map[string]interface{})
	if l, ok := summary.([]interface{}); ok && len(l) > 0 {
		s, _ = l[0].(map[string]interface{})
	}
	str := func(key string) string {
		v, _ := s[key].(string)
		return strings.TrimSpace(v)
	}
	for _, pair := range [][2]string{{"upgrade_human_version", "upgrade_version"}, {"latest_human_version", "latest_version"}} {
		human, catalog := str(pair[0]), str(pair[1])
		switch {
		case human != "" && catalog != "" && human != catalog:
			return fmt.Sprintf("%s (catalog version %s, latest)", human, catalog)
		case human != "":
			return human + " (latest)"
		case catalog != "":
			return catalog + " (latest)"
		}
	}
	return "the latest available version (app.upgrade_summary did not name it)"
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

// isNoUpgradeAvailable reports whether err is the middleware's "No upgrade
// available" error. app.upgrade_summary (and app.upgrade) raise it as EFAULT,
// with a traceback, when the app has no newer version to upgrade to (on some
// TrueNAS versions also when the app's catalog entry is missing). A requested
// version that is not an upgrade is a separate validation error.
func isNoUpgradeAvailable(err error) bool {
	var apiErr *truenas.APICallError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "no upgrade available")
}

// noUpgradeAvailableError is the short error returned instead of the raw
// middleware error (request block and traceback) when there is no upgrade to
// apply. It names the installed version when app (an app.query entry) is
// known.
func noUpgradeAvailableError(appName, version string, app map[string]interface{}) error {
	var b strings.Builder
	if version == "latest" {
		fmt.Fprintf(&b, "no upgrade available for app %q", appName)
	} else {
		fmt.Fprintf(&b, "no upgrade to version %q is available for app %q", version, appName)
	}
	if installed, _ := app["human_version"].(string); strings.TrimSpace(installed) != "" {
		fmt.Fprintf(&b, " (installed version %s)", strings.TrimSpace(installed))
	}
	b.WriteString("; nothing was changed")
	return errors.New(b.String())
}

// queryAppQuietly returns the app.query entry for appName, or nil if the
// query fails or finds nothing. It is only used to add detail to a message.
func queryAppQuietly(ctx context.Context, client *truenas.Client, appName string) map[string]interface{} {
	result, err := client.Call(ctx, "app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return nil
	}
	var apps []map[string]interface{}
	if err := json.Unmarshal(result, &apps); err != nil || len(apps) == 0 {
		return nil
	}
	return apps[0]
}
