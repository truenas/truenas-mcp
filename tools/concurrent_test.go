package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// CallTool and ListTools are safe for concurrent use once the policy is set:
// the server runs tool calls concurrently. Run with -race.
func TestRegistryConcurrentCalls(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	if err := r.SetPolicy(Policy{Deny: []string{"system_reboot"}}); err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 16, 5
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			ctx := context.Background()
			for range rounds {
				if out, err := r.CallTool(ctx, "system_info", nil); err != nil || !strings.Contains(out, `"hostname": "nas"`) {
					t.Errorf("system_info = %q, %v", out, err)
				}
				if _, err := r.CallTool(ctx, "query_pools", map[string]interface{}{"padding": 1}); err == nil || !strings.Contains(err.Error(), "unknown argument") {
					t.Errorf("unknown argument: %v", err)
				}
				if _, err := r.CallTool(ctx, "system_reboot", nil); err == nil || !strings.Contains(err.Error(), "deny-tools") {
					t.Errorf("denied tool: %v", err)
				}
				if _, err := r.CallTool(ctx, "nope", nil); err == nil {
					t.Error("unknown tool accepted")
				}
				if len(r.ListTools()) == 0 {
					t.Error("no tools listed")
				}
			}
		})
	}
	wg.Wait()
	if got := len(fake.called("system.info")); got != workers*rounds {
		t.Errorf("system.info was called %d times, want %d", got, workers*rounds)
	}
}

// A call whose context is already done runs nothing.
func TestCallToolWithDoneContextRunsNothing(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.CallTool(ctx, "system_info", nil)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "was not run") {
		t.Errorf("CallTool = %v; want a not-run error wrapping context.Canceled", err)
	}
	if calls := fake.called("system.info"); len(calls) != 0 {
		t.Errorf("system.info was called %d times", len(calls))
	}
}
