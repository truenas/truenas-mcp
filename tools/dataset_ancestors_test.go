package tools

import (
	"encoding/json"
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
	out, err := handleCreateDataset(client, args)
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

func ancestorStrings(preview map[string]interface{}) []string {
	raw, ok := preview["ancestors_to_create"]
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
		got := ancestorStrings(preview)
		want := []string{"tank/apps", "tank/apps/jellyfin"}
		if !sameStrings(got, want) {
			t.Errorf("ancestors_to_create = %v, want %v (parent to child)", got, want)
		}
		payload, _ := preview["payload"].(map[string]interface{})
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
		got := ancestorStrings(preview)
		want := []string{"tank/apps/jellyfin"}
		if !sameStrings(got, want) {
			t.Errorf("ancestors_to_create = %v, want %v", got, want)
		}
	})

	t.Run("empty list when all ancestors exist", func(t *testing.T) {
		client, _ := ancestorDryRunFake(t, []string{"tank", "tank/apps", "tank/apps/jellyfin"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/apps/jellyfin/config",
		})
		got := ancestorStrings(preview)
		if !sameStrings(got, []string{}) {
			t.Errorf("ancestors_to_create = %v, want an empty list", got)
		}
	})

	t.Run("empty list when there are no intermediate ancestors", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name": "tank/config",
		})
		got := ancestorStrings(preview)
		if !sameStrings(got, []string{}) {
			t.Errorf("ancestors_to_create = %v, want an empty list", got)
		}
		if n := len(fake.called("pool.dataset.query")); n != 0 {
			t.Errorf("queried ancestors %d times for a pool/leaf name, want 0", n)
		}
	})

	t.Run("does not list ancestors when create_ancestors is false", func(t *testing.T) {
		client, fake := ancestorDryRunFake(t, []string{"tank"})
		preview := createDatasetDryRun(t, client, map[string]interface{}{
			"name":             "tank/apps/jellyfin/config",
			"create_ancestors": false,
		})
		got := ancestorStrings(preview)
		if len(got) != 0 {
			t.Errorf("create_ancestors=false but ancestors_to_create = %v, want none", got)
		}
		payload, _ := preview["payload"].(map[string]interface{})
		if payload["create_ancestors"] != false {
			t.Errorf("payload create_ancestors = %v, want false", payload["create_ancestors"])
		}
		if n := len(fake.called("pool.dataset.create")); n != 0 {
			t.Errorf("dry-run called pool.dataset.create %d times", n)
		}
	})
}
