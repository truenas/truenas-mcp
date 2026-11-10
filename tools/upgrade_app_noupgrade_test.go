package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// noUpgradeRegistry is an upgrade_app registry whose middleware answers the
// methods in failing with the "No upgrade available" error TrueNAS raises
// for an app with no newer version (EFAULT, with a traceback).
func noUpgradeRegistry(t *testing.T, failing ...string) (*Registry, *fakeMiddleware) {
	t.Helper()
	app := appEntry("plex", "RUNNING", [2]string{"web", "running"})
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"app.upgrade_summary": upgradeSummaryFixture("x"),
			"app.upgrade":         upgradeJobID,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			for _, m := range failing {
				if method == m {
					return nil, middlewareError("EFAULT", "No upgrade available for 'plex'"), true
				}
			}
			if method == "app.query" {
				return []interface{}{app}, nil, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func assertNoUpgradeError(t *testing.T, err error, out string, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got output %s", out)
	}
	msg := err.Error()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error should contain %q, got %q", w, msg)
		}
	}
	for _, leak := range []string{"Traceback", "Trace:", "Request:", "Params:", "EFAULT", "(code ", "\n"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error should be short and free of middleware detail, found %q in %q", leak, msg)
		}
	}
}

func TestUpgradeAppNoUpgradeAvailablePreview(t *testing.T) {
	for name, args := range map[string]map[string]interface{}{
		"dry_run":    {"app_name": "plex", "dry_run": true},
		"no confirm": {"app_name": "plex"},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake := noUpgradeRegistry(t, "app.upgrade_summary")
			out, err := r.CallTool(t.Context(), "upgrade_app", args)
			assertNoUpgradeError(t, err, out, `no upgrade available for app "plex" (installed version 2.0.0_1.2.0)`, "nothing was changed")
			if n := len(fake.called("app.upgrade")); n != 0 {
				t.Errorf("preview called app.upgrade %d times", n)
			}
		})
	}
}

func TestUpgradeAppNoUpgradeAvailableExecute(t *testing.T) {
	for _, failing := range []string{"app.upgrade_summary", "app.upgrade"} {
		t.Run(failing, func(t *testing.T) {
			r, fake := noUpgradeRegistry(t, failing)
			out, err := r.CallTool(t.Context(), "upgrade_app", approve(t, r, "upgrade_app", map[string]interface{}{"app_name": "plex"}))
			assertNoUpgradeError(t, err, out, `no upgrade available for app "plex"`, "(installed version 2.0.0_1.2.0)")
			if failing == "app.upgrade_summary" {
				if n := len(fake.called("app.upgrade")); n != 0 {
					t.Errorf("app.upgrade called %d times after the summary said no upgrade", n)
				}
			}
		})
	}
}

func TestUpgradeAppNoUpgradeToRequestedVersion(t *testing.T) {
	r, _ := noUpgradeRegistry(t, "app.upgrade_summary")
	out, err := r.CallTool(t.Context(), "upgrade_app", map[string]interface{}{"app_name": "plex", "version": "9.9.9", "dry_run": true})
	assertNoUpgradeError(t, err, out, `no upgrade to version "9.9.9" is available for app "plex" (installed version 2.0.0_1.2.0)`)
}

// Other upgrade_summary failures keep the full middleware error.
func TestUpgradeAppOtherSummaryErrorUnchanged(t *testing.T) {
	fake := &fakeMiddleware{
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return []interface{}{appEntry("plex", "RUNNING")}, nil, true
			case "app.upgrade_summary":
				return nil, middlewareError("EINVAL", "Catalog is not synced"), true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	_, err := (&upgradeAppDryRun{}).ExecuteDryRun(context.Background(), client, map[string]interface{}{"app_name": "plex"})
	if err == nil || !strings.Contains(err.Error(), "failed to get upgrade summary") || !strings.Contains(err.Error(), "Catalog is not synced") {
		t.Fatalf("want the middleware error passed through, got %v", err)
	}
	if isNoUpgradeAvailable(err) {
		t.Error("EINVAL error classified as no upgrade available")
	}
}

// Middleware errors that only resemble "No upgrade available" keep the full
// error: they are not classified as no-upgrade.
func TestIsNoUpgradeAvailableNearMisses(t *testing.T) {
	for _, reason := range []string{
		"Upgrade available for 'plex'",
		"An upgrade is available for 'plex'",
		"No upgrade path available for 'plex'",
		"No upgrades available for 'plex'",
		"No upgrade summary available for 'plex'",
		"No catalog available for 'plex'",
	} {
		t.Run(reason, func(t *testing.T) {
			fake := &fakeMiddleware{
				handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
					switch method {
					case "app.query":
						return []interface{}{appEntry("plex", "RUNNING")}, nil, true
					case "app.upgrade_summary":
						return nil, middlewareError("EFAULT", reason), true
					}
					return nil, nil, false
				},
			}
			client, _ := newFakeClientFrom(t, fake)
			_, err := (&upgradeAppDryRun{}).ExecuteDryRun(context.Background(), client, map[string]interface{}{"app_name": "plex", "dry_run": true})
			if err == nil {
				t.Fatal("expected an error")
			}
			if isNoUpgradeAvailable(err) {
				t.Errorf("%q classified as no upgrade available", reason)
			}
			if !strings.Contains(err.Error(), "failed to get upgrade summary") || !strings.Contains(err.Error(), reason) {
				t.Errorf("want the middleware error passed through, got %v", err)
			}
		})
	}
}

func TestIsNoUpgradeAvailableNeedsAPIError(t *testing.T) {
	if isNoUpgradeAvailable(errors.New("No upgrade available")) {
		t.Error("a non-API error must not be classified")
	}
	if isNoUpgradeAvailable(nil) {
		t.Error("nil must not be classified")
	}
}

func TestNoUpgradeAvailableErrorWithoutApp(t *testing.T) {
	got := noUpgradeAvailableError("plex", "latest", nil).Error()
	if got != `no upgrade available for app "plex"; nothing was changed` {
		t.Errorf("got %q", got)
	}
}
