package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/truenas"
)

func ancestorDryRunFake(t *testing.T, existing []string) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	have := map[string]bool{}
	for _, n := range existing {
		have[n] = true
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"pool.query": []interface{}{map[string]interface{}{"name": "tank", "status": "ONLINE"}},
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			var args []interface{}
			if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
				return []interface{}{}, nil, true
			}
			filters, _ := args[0].([]interface{})
			name := ""
			if len(filters) > 0 {
				if f, ok := filters[0].([]interface{}); ok && len(f) >= 3 {
					if s, ok := f[2].(string); ok {
						name = s
					}
				}
			}
			if have[name] {
				return []interface{}{map[string]interface{}{"name": name}}, nil, true
			}
			return []interface{}{}, nil, true
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	return client, fake
}

func createDatasetDryRun(t *testing.T, client *truenas.Client, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	args["dry_run"] = true
	out, err := handleCreateDataset(context.Background(), client, args)
	if err != nil {
		t.Fatalf("create_dataset dry-run: %v", err)
	}
	var preview map[string]interface{}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("parse preview: %v; body=%s", err, out)
	}
	if preview["dry_run"] != true {
		t.Fatalf("not a dry run: %s", out)
	}
	return preview
}

func datasetPreviewMap(preview map[string]interface{}, key string) (interface{}, bool) {
	if raw, ok := preview[key]; ok {
		return raw, true
	}
	if cs, ok := preview["current_state"].(map[string]interface{}); ok {
		if raw, ok := cs[key]; ok {
			return raw, true
		}
	}
	return nil, false
}

func datasetPreviewPayload(preview map[string]interface{}) map[string]interface{} {
	raw, _ := datasetPreviewMap(preview, "payload")
	m, _ := raw.(map[string]interface{})
	return m
}

func previewAncestorsField(preview map[string]interface{}) (interface{}, bool) {
	if raw, ok := preview["ancestors_to_create"]; ok {
		return raw, true
	}
	if cs, ok := preview["current_state"].(map[string]interface{}); ok {
		if raw, ok := cs["ancestors_to_create"]; ok {
			return raw, true
		}
	}
	return nil, false
}

func ancestorStrings(preview map[string]interface{}) []string {
	raw, ok := previewAncestorsField(preview)
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func requireAncestors(t *testing.T, preview map[string]interface{}, want []string) {
	t.Helper()
	raw, ok := previewAncestorsField(preview)
	if !ok {
		t.Fatalf("ancestors_to_create is missing, want %v", want)
	}
	if raw == nil {
		t.Fatalf("ancestors_to_create is null, want %v", want)
	}
	got := ancestorStrings(preview)
	if !sameStrings(got, want) {
		t.Errorf("ancestors_to_create = %v, want %v", got, want)
	}
}
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestCreateDatasetDryRunListsMissingAncestors is the regression for the
// create_dataset dry-run preview listing missing parent datasets that
// create_ancestors (default true) would create, parent to child.
func TestCreateDatasetDryRunListsMissingAncestors(t *testing.T) {
	t.Run("lists missing ancestors parent to child", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/apps/jellyfin/config",
		})
		requireAncestors(t, preview, []string{"tank/apps", "tank/apps/jellyfin"})
		payload := datasetPreviewPayload(preview)
		if payload["create_ancestors"] != true {
			t.Errorf("default create_ancestors = %v, want true", payload["create_ancestors"])
		}
		if n := len(fake.called("pool.dataset.create")); n != 0 {
			t.Errorf("dry-run called pool.dataset.create %d times", n)
		}
	})

	t.Run("omits ancestors that already exist", func(t *testing.T) {
		client, _ := ancestorDryRunFake(t, []string{"tank", "tank/apps"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/apps/jellyfin/config",
		})
		requireAncestors(t, preview, []string{"tank/apps/jellyfin"})
	})

	t.Run("empty list when all ancestors exist", func(t *testing.T) {
		client, _ := ancestorDryRunFake(t, []string{"tank", "tank/apps", "tank/apps/jellyfin"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/apps/jellyfin/config",
		})
		requireAncestors(t, preview, []string{})
	})

	t.Run("empty list when there are no intermediate ancestors", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/config",
		})
		requireAncestors(t, preview, []string{})
		// Preflight may query the target once to see if it already exists;
		// there are no intermediate ancestors to look up.
		if n := len(fake.called("pool.dataset.query")); n > 1 {
			t.Errorf("queried datasets %d times for a pool/leaf name, want at most 1", n)
		}
	})

	t.Run("does not list ancestors when create_ancestors is false", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name":             "tank/apps/jellyfin/config",
			"create_ancestors": false,
		})
		requireAncestors(t, preview, []string{})
		payload := datasetPreviewPayload(preview)
		if payload["create_ancestors"] != false {
			t.Errorf("payload create_ancestors = %v, want false", payload["create_ancestors"])
		}
		if n := len(fake.called("pool.dataset.create")); n != 0 {
			t.Errorf("dry-run called pool.dataset.create %d times", n)
		}
	})

	t.Run("exact match not a prefix sibling", func(t *testing.T) {
		client, _ := ancestorDryRunFake(t, []string{"tank", "tank/ab"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/a/b",
		})
		requireAncestors(t, preview, []string{"tank/a"})
	})

	t.Run("notes that ancestors inherit and are unencrypted under an unencrypted parent", func(t *testing.T) {
		client, _ := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name":       "tank/apps/jellyfin/config",
			"encryption": true,
			"encryption_options": map[string]interface{}{
				"generate_key": true,
			},
		})
		requireAncestors(t, preview, []string{"tank/apps", "tank/apps/jellyfin"})
		rawNote, _ := datasetPreviewMap(preview, "ancestors_note")
		note, _ := rawNote.(string)
		low := strings.ToLower(note)
		for _, want := range []string{"inherit", "encryption", "unencrypted", "no quota of their own"} {
			if !strings.Contains(low, want) {
				t.Errorf("ancestors_note = %q, want it to mention %q", note, want)
			}
		}
		if strings.Contains(low, "inherit") && strings.Contains(low, "quota") && !strings.Contains(low, "no quota") {
			t.Errorf("ancestors_note = %q still says quota is inherited", note)
		}
	})
}

// TestCreateDatasetDryRunRejectsNamesTheCreateWouldReject is the follow-up
// for the ancestor preview listing parents for a name the middleware would
// refuse: a missing pool, an existing target, '.'/'..' components, and a
// name over the ZFS length limit.
func TestCreateDatasetDryRunRejectsNamesTheCreateWouldReject(t *testing.T) {
	t.Run("missing pool", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{})
		_, err := createDatasetDryRunMaybe(t, client, map[string]interface{}{
			"name": "nopool/a/b/c",
		})
		if err == nil {
			t.Fatal("dry-run of nopool/a/b/c succeeded")
		}
		if !strings.Contains(err.Error(), "nopool") || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error %q, want a missing-pool message", err)
		}
		if n := len(fake.called("pool.dataset.create")); n != 0 {
			t.Errorf("dry-run called pool.dataset.create %d times", n)
		}
	})

	t.Run("target already exists", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank", "tank/apps", "tank/apps/jellyfin", "tank/apps/jellyfin/config"})
		_, err := createDatasetDryRunMaybe(t, client, map[string]interface{}{
			"name": "tank/apps/jellyfin/config",
		})
		if err == nil {
			t.Fatal("dry-run of an existing dataset succeeded")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("error %q, want already exists", err)
		}
		if n := len(fake.called("pool.dataset.create")); n != 0 {
			t.Errorf("dry-run called pool.dataset.create %d times", n)
		}
	})

	t.Run("dot-dot components", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		_, err := createDatasetDryRunMaybe(t, client, map[string]interface{}{
			"name": "tank/../x/y",
		})
		if err == nil {
			t.Fatal("dry-run of tank/../x/y succeeded")
		}
		if !strings.Contains(err.Error(), "'.' or '..'") {
			t.Errorf("error %q, want a '.'/'..' message", err)
		}
		if n := len(fake.called("pool.dataset.query")); n != 0 {
			t.Errorf("queried datasets %d times for an invalid name", n)
		}
	})
}

func createDatasetDryRunMaybe(t *testing.T, client *truenas.Client, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	args["dry_run"] = true
	out, err := handleCreateDataset(context.Background(), client, args)
	if err != nil {
		return nil, err
	}
	var preview map[string]interface{}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("parse preview: %v; body=%s", err, out)
	}
	return preview, nil
}
