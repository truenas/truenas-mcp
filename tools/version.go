package tools

import (
	"fmt"
	"sync"

	"github.com/truenas/truenas-mcp/truenas"
)

// VersionRange is the TrueNAS releases a tool is available on. Both ends
// are inclusive. A zero Min means MinSupported (25.10). A zero Max means
// there is no upper bound.
type VersionRange struct {
	Min truenas.Version
	Max truenas.Version
}

// toolVersion is the per-tool min/max table used to hide and refuse tools
// that the connected TrueNAS release cannot run. Every registered tool has
// an entry. Currently all tools are available on 25.10 and later; entries
// with a higher Min or a Max are how 27-only or 25.10-only methods are
// gated when they appear.
var toolVersionMu sync.RWMutex
var toolVersion = map[string]VersionRange{
	// System
	"system_info":     {Min: truenas.MinSupported},
	"system_health":   {Min: truenas.MinSupported},
	"check_updates":   {Min: truenas.MinSupported},
	"update_status":   {Min: truenas.MinSupported},
	"download_update": {Min: truenas.MinSupported},
	"apply_update":    {Min: truenas.MinSupported},
	"system_reboot":   {Min: truenas.MinSupported},
	// Boot environments
	"query_boot_environments":      {Min: truenas.MinSupported},
	"get_current_boot_environment": {Min: truenas.MinSupported},
	"delete_boot_environment":      {Min: truenas.MinSupported},
	// Scrubs
	"query_scrub_schedules": {Min: truenas.MinSupported},
	"get_scrub_status":      {Min: truenas.MinSupported},
	"create_scrub_schedule": {Min: truenas.MinSupported},
	"run_scrub":             {Min: truenas.MinSupported},
	"delete_scrub_schedule": {Min: truenas.MinSupported},
	// Directory services
	"get_directory_service_status": {Min: truenas.MinSupported},
	"query_directory_services":     {Min: truenas.MinSupported},
	"list_directory_certificates":  {Min: truenas.MinSupported},
	"refresh_directory_cache":      {Min: truenas.MinSupported},
	"configure_directory_service":  {Min: truenas.MinSupported},
	"leave_directory_service":      {Min: truenas.MinSupported},
	// Storage and shares
	"query_pools":               {Min: truenas.MinSupported},
	"query_datasets":            {Min: truenas.MinSupported},
	"query_snapshots":           {Min: truenas.MinSupported},
	"query_shares":              {Min: truenas.MinSupported},
	"query_vms":                 {Min: truenas.MinSupported},
	"create_dataset":            {Min: truenas.MinSupported},
	"create_smb_share":          {Min: truenas.MinSupported},
	"create_nfs_share":          {Min: truenas.MinSupported},
	"analyze_capacity":          {Min: truenas.MinSupported},
	"get_pool_capacity_details": {Min: truenas.MinSupported},
	// Alerts
	"list_alerts":   {Min: truenas.MinSupported},
	"dismiss_alert": {Min: truenas.MinSupported},
	"restore_alert": {Min: truenas.MinSupported},
	// Metrics
	"get_system_metrics":  {Min: truenas.MinSupported},
	"get_network_metrics": {Min: truenas.MinSupported},
	"get_disk_metrics":    {Min: truenas.MinSupported},
	"get_arc_metrics":     {Min: truenas.MinSupported},
	"get_ups_metrics":     {Min: truenas.MinSupported},
	// Apps
	"query_apps":              {Min: truenas.MinSupported},
	"get_app_config":          {Min: truenas.MinSupported},
	"search_app_catalog":      {Min: truenas.MinSupported},
	"get_app_catalog_details": {Min: truenas.MinSupported},
	"install_app":             {Min: truenas.MinSupported},
	"update_app":              {Min: truenas.MinSupported},
	"upgrade_app":             {Min: truenas.MinSupported},
	"start_app":               {Min: truenas.MinSupported},
	"stop_app":                {Min: truenas.MinSupported},
	"delete_app":              {Min: truenas.MinSupported},
	// Jobs and local task tracking
	"query_jobs": {Min: truenas.MinSupported},
	"tasks_list": {Min: truenas.MinSupported},
	"tasks_get":  {Min: truenas.MinSupported},
}

func versionRangeOf(name string) VersionRange {
	toolVersionMu.RLock()
	defer toolVersionMu.RUnlock()
	if r, ok := toolVersion[name]; ok {
		return r
	}
	// A tool missing from the table is treated as 25.10+ with no maximum,
	// matching today's default. TestEveryToolHasVersionRange fails if a
	// registered tool is absent.
	return VersionRange{Min: truenas.MinSupported}
}

func (r VersionRange) min() truenas.Version {
	if r.Min.IsZero() {
		return truenas.MinSupported
	}
	return r.Min
}

// setToolVersionForTest overrides name's range. Tests must restore the
// previous value (ok reports whether there was one).
func setToolVersionForTest(name string, rng VersionRange) (prev VersionRange, ok bool) {
	toolVersionMu.Lock()
	defer toolVersionMu.Unlock()
	prev, ok = toolVersion[name]
	toolVersion[name] = rng
	return prev, ok
}

func restoreToolVersionForTest(name string, prev VersionRange, ok bool) {
	toolVersionMu.Lock()
	defer toolVersionMu.Unlock()
	if ok {
		toolVersion[name] = prev
	} else {
		delete(toolVersion, name)
	}
}

// checkVersion refuses a tool call when the connected TrueNAS is older
// than 25.10, or when the tool is outside its version range. A nil client
// (unit tests without a NAS) skips the gate. The version is loaded on the
// first call and re-read after a reconnect.
func (r *Registry) checkVersion(name string) error {
	if r.client == nil {
		return nil
	}
	v, err, ok := r.client.SessionVersion()
	if !ok {
		// Production loads the version at startup. Tests that never
		// call RequireSupportedVersion skip the gate.
		return nil
	}
	if err != nil {
		return err
	}
	if v.Less(truenas.MinSupported) {
		return &truenas.UnsupportedVersionError{Reported: v.Raw, Parsed: v}
	}
	return toolUnavailableError(name, v)
}

// toolHiddenByVersion reports whether ListTools should hide name, using
// only a version that is already cached so listing does not contact
// TrueNAS. Production loads the version at startup before tools/list.
func (r *Registry) toolHiddenByVersion(name string) bool {
	if r.client == nil {
		return false
	}
	v, err, ok := r.client.CachedServerVersion()
	if !ok || err != nil {
		return false
	}
	if v.Less(truenas.MinSupported) {
		return false
	}
	return toolUnavailableError(name, v) != nil
}

func toolUnavailableError(name string, v truenas.Version) error {
	rng := versionRangeOf(name)
	min := rng.min()
	if v.Less(min) {
		return fmt.Errorf("tool %s is not available on TrueNAS %s (requires %s or later); nothing was executed", name, v, min)
	}
	if !rng.Max.IsZero() && rng.Max.Less(v) {
		return fmt.Errorf("tool %s is not available on TrueNAS %s (requires %s or earlier); nothing was executed", name, v, rng.Max)
	}
	return nil
}
