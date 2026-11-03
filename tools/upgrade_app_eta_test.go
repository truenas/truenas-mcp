package tools

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The estimate covers the upgrade job plus the configured health wait, so a
// larger health_timeout_seconds raises max_seconds.
func TestUpgradeAppEstimatedTimeFollowsHealthTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  map[string]interface{}
		health int
	}{
		{"default", nil, defaultUpgradeHealthTimeoutSeconds},
		{"zero", map[string]interface{}{"health_timeout_seconds": 0}, 0},
		{"custom 450", map[string]interface{}{"health_timeout_seconds": 450}, 450},
		{"maximum", map[string]interface{}{"health_timeout_seconds": maxUpgradeHealthTimeoutSeconds}, maxUpgradeHealthTimeoutSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previewPaths(t, func(t *testing.T, explicit bool) {
				r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
				preview, _ := upgradePreview(t, r, explicit, tc.extra)
				est, ok := preview["estimated_time"].(map[string]interface{})
				if !ok {
					t.Fatalf("no estimated_time in %v", preview)
				}
				maxS, minS := est["max_seconds"].(float64), est["min_seconds"].(float64)
				if want := float64(maxUpgradeJobSeconds + tc.health); maxS != want {
					t.Errorf("max_seconds = %v, want %v", maxS, want)
				}
				if maxS < float64(tc.health) || minS > maxS {
					t.Errorf("inconsistent estimate: min %v, max %v, health wait %d", minS, maxS, tc.health)
				}
				note, _ := est["note"].(string)
				if !strings.Contains(note, strconv.Itoa(tc.health)+"s waiting for the app to report RUNNING") {
					t.Errorf("note should name the health wait: %q", note)
				}
				if got := plannedStep(t, preview, "verify")["details"].(map[string]interface{})["health_timeout_seconds"]; got != float64(tc.health) {
					t.Errorf("verify step health_timeout_seconds = %v, want %d", got, tc.health)
				}
			})
		})
	}
}

func TestUpgradeEstimatedTime(t *testing.T) {
	if got := upgradeEstimatedTime(0); got.MinSeconds != minUpgradeJobSeconds || got.MaxSeconds != maxUpgradeJobSeconds {
		t.Errorf("no health wait: got %+v", got)
	}
	if got := upgradeEstimatedTime(defaultUpgradeHealthTimeoutSeconds * time.Second); got.MaxSeconds != maxUpgradeJobSeconds+defaultUpgradeHealthTimeoutSeconds {
		t.Errorf("default health wait: got %+v", got)
	}
}
