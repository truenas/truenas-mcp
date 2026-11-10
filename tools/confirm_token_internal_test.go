package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// fakeClock is an injectable clock for the token store.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func clockedRegistry(t *testing.T) (*Registry, map[string][]map[string]interface{}, *fakeClock) {
	t.Helper()
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r.confirmTokens.now = clock.now
	return r, seen, clock
}

func previewToken(t *testing.T, r *Registry, name string, args map[string]interface{}) string {
	t.Helper()
	out, err := r.CallTool(t.Context(), name, args)
	if err != nil {
		t.Fatalf("%s preview: %v", name, err)
	}
	tok := replyToken(t, out)
	if tok == "" {
		t.Fatalf("%s preview has no token:\n%s", name, out)
	}
	return tok
}

// A token is valid for exactly five minutes.
func TestConfirmTokenExpiry(t *testing.T) {
	r, seen, clock := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}

	tok := previewToken(t, r, "stop_app", args)
	clock.advance(confirmationTokenTTL - time.Second)
	if _, err := r.CallTool(t.Context(), "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
		t.Fatalf("token just before expiry refused: %v", err)
	}

	tok = previewToken(t, r, "stop_app", args)
	clock.advance(confirmationTokenTTL)
	_, err := r.CallTool(t.Context(), "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("expired token: err = %v", err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times, want 1 (only the unexpired token)", n)
	}
}

// A rejected token is an error whose reply carries the reason, a fresh
// preview and a fresh token that works.
func TestConfirmTokenRejectionCarriesFreshToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	_, err := r.CallTool(t.Context(), "delete_app", tokenArgs(args, map[string]interface{}{"confirmation_token": "0123456789abcdef0123456789abcdef"}))
	var reply *mcp.ReplyError
	if !errors.As(err, &reply) {
		t.Fatalf("err = %v, want a *mcp.ReplyError", err)
	}
	if strings.Contains(reply.Message, "0123456789abcdef") {
		t.Errorf("error message echoes the presented token: %s", reply.Message)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(reply.Reply), &body); err != nil {
		t.Fatal(err)
	}
	fresh, _ := body["confirmation_token"].(string)
	if body["status"] != "confirmation_required" || body["executed"] != false || body["preview"] == nil || fresh == "" ||
		!strings.Contains(fmt.Sprint(body["error"]), "unknown") {
		t.Fatalf("rejection reply = %s", reply.Reply)
	}
	if strings.Contains(reply.Message, fresh) {
		t.Error("the error message (which is logged and redacted as text) carries the fresh token")
	}
	if _, err := r.CallTool(t.Context(), "delete_app", tokenArgs(args, map[string]interface{}{"confirmation_token": fresh})); err != nil {
		t.Fatalf("fresh token refused: %v", err)
	}
	if n := realCalls(seen["delete_app"]); n != 1 {
		t.Errorf("delete_app ran %d times, want 1", n)
	}
}

// A blocked preview is a ReplyError with status blocked and no token, and
// leaves no token behind.
func TestConfirmTokenBlockedReplyHasNoToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	tool := r.tools["delete_boot_environment"]
	tool.Handler = func(_ context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
		seen["delete_boot_environment"] = append(seen["delete_boot_environment"], args)
		return `{"tool":"delete_boot_environment","warnings":["BLOCKED: Cannot delete active boot environment 'x' (currently running)"]}`, nil
	}
	tool.Plan = stubToolPlan("delete_boot_environment")
	r.tools["delete_boot_environment"] = tool
	_, err := r.CallTool(t.Context(), "delete_boot_environment", map[string]interface{}{"id": "x"})
	var reply *mcp.ReplyError
	if !errors.As(err, &reply) {
		t.Fatalf("err = %v, want a *mcp.ReplyError", err)
	}
	if strings.Contains(reply.Reply, "confirmation_token") || !strings.Contains(reply.Reply, `"status": "blocked"`) ||
		!strings.Contains(reply.Message, "currently running") || strings.Contains(reply.Message, "call delete_boot_environment again") {
		t.Errorf("blocked reply: %s / %s", reply.Message, reply.Reply)
	}
	if n := len(r.confirmTokens.entries); n != 0 {
		t.Errorf("%d tokens issued for a blocked preview", n)
	}
	if n := realCalls(seen["delete_boot_environment"]); n != 0 {
		t.Errorf("blocked call executed %d times", n)
	}
}

// A failed preview issues no token.
func TestConfirmTokenPreviewFailureIssuesNoToken(t *testing.T) {
	r, _, _ := clockedRegistry(t)
	tool := r.tools["delete_app"]
	tool.Handler = func(context.Context, *truenas.Client, map[string]interface{}) (string, error) {
		return "", errors.New("app not found: plex")
	}
	r.tools["delete_app"] = tool
	if _, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex"}); err == nil {
		t.Fatal("failed preview returned no error")
	}
	if n := len(r.confirmTokens.entries); n != 0 {
		t.Errorf("%d tokens issued for a failed preview", n)
	}
}

// Tokens are 128-bit random hex strings, all different.
func TestConfirmTokenFormat(t *testing.T) {
	c := newConfirmationTokens()
	h, _ := confirmationArgsHash(nil)
	seen := map[string]bool{}
	hexToken := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for i := 0; i < 200; i++ {
		tok, err := c.issue("stop_app", h)
		if err != nil {
			t.Fatal(err)
		}
		if !hexToken.MatchString(tok) || seen[tok] {
			t.Fatalf("token %d = %q (duplicate=%v)", i, tok, seen[tok])
		}
		seen[tok] = true
	}
}

// The argument hash ignores key order and the control arguments, and sees
// every other difference, nested ones included.
func TestConfirmationArgsHash(t *testing.T) {
	h := func(args map[string]interface{}) [32]byte {
		t.Helper()
		sum, err := confirmationArgsHash(args)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	base := h(map[string]interface{}{"a": 1.0, "b": map[string]interface{}{"x": "1", "y": []interface{}{1.0, 2.0}}})
	same := h(map[string]interface{}{"b": map[string]interface{}{"y": []interface{}{1.0, 2.0}, "x": "1"}, "a": 1.0,
		"confirm": true, "dry_run": false, "confirmation_token": "t"})
	if base != same {
		t.Error("key order or control arguments changed the hash")
	}
	for _, other := range []map[string]interface{}{
		{"a": 2.0, "b": map[string]interface{}{"x": "1", "y": []interface{}{1.0, 2.0}}},
		{"a": 1.0, "b": map[string]interface{}{"x": "1", "y": []interface{}{2.0, 1.0}}},
		{"a": 1.0, "b": map[string]interface{}{"x": 1.0, "y": []interface{}{1.0, 2.0}}},
		{"a": 1.0, "b": map[string]interface{}{"x": "1", "y": []interface{}{1.0, 2.0}}, "c": nil},
		{"a": "1", "b": map[string]interface{}{"x": "1", "y": []interface{}{1.0, 2.0}}},
	} {
		if h(other) == base {
			t.Errorf("hash collision for %v", other)
		}
	}
}

// A null optional argument means omitted, so it hashes like the omitted one.
func TestConfirmTokenNullOptionalMatchesOmitted(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	tok := previewToken(t, r, "delete_app", map[string]interface{}{"app_name": "plex"})
	if _, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "remove_images": nil, "confirmation_token": tok}); err != nil {
		t.Fatalf("null optional argument changed the binding: %v", err)
	}
	if n := realCalls(seen["delete_app"]); n != 1 {
		t.Errorf("delete_app ran %d times", n)
	}
}

// Concurrent calls with one token execute exactly once.
func TestConfirmTokenSingleUseUnderConcurrency(t *testing.T) {
	r := NewRegistry(nil, nil)
	var ran atomic.Int32
	tool := r.tools["stop_app"]
	tool.Handler = func(_ context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
		if args["dry_run"] != true {
			ran.Add(1)
		}
		return `{"ok":true}`, nil
	}
	tool.Plan = stubToolPlan("stop_app")
	r.tools["stop_app"] = tool
	tok := previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex", "confirmation_token": tok}); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ran.Load() != 1 || ok.Load() != 1 {
		t.Errorf("one token executed %d times (%d successes), want 1", ran.Load(), ok.Load())
	}
}

// The store is bounded: at the cap the token closest to expiry is dropped.
func TestConfirmTokenStoreBounded(t *testing.T) {
	c := newConfirmationTokens()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.now = clock.now
	h, _ := confirmationArgsHash(nil)
	first, _ := c.issue("stop_app", h)
	for i := 0; i < maxConfirmationTokens+10; i++ {
		clock.advance(time.Millisecond)
		if _, err := c.issue("stop_app", h); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.entries); n > maxConfirmationTokens {
		t.Errorf("%d tokens held, cap %d", n, maxConfirmationTokens)
	}
	if err := c.redeem(first, "stop_app", h); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("oldest token after the cap: err = %v", err)
	}
	// Expired entries are dropped on the next issue.
	clock.advance(confirmationTokenTTL)
	if _, err := c.issue("stop_app", h); err != nil {
		t.Fatal(err)
	}
	if n := len(c.entries); n != 1 {
		t.Errorf("%d tokens held after all expired, want 1", n)
	}
}

// IsConfirmationToken accepts only a live token this registry issued.
func TestIsConfirmationToken(t *testing.T) {
	r, _, clock := clockedRegistry(t)
	tok := previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	if !r.IsConfirmationToken(tok) {
		t.Error("issued token not recognised")
	}
	if r.IsConfirmationToken("0123456789abcdef0123456789abcdef") || r.IsConfirmationToken("") {
		t.Error("an unissued value was recognised")
	}
	if NewRegistry(nil, nil).IsConfirmationToken(tok) {
		t.Error("another registry recognised the token")
	}
	clock.advance(confirmationTokenTTL)
	if r.IsConfirmationToken(tok) {
		t.Error("expired token recognised")
	}
	tok = previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	if _, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex", "confirmation_token": tok}); err != nil {
		t.Fatal(err)
	}
	if r.IsConfirmationToken(tok) {
		t.Error("used token recognised")
	}
}

// confirmation_token is the first key of every reply that carries one, so
// the server's output cap (which keeps the start) never cuts it.
func TestConfirmationTokenIsFirstKey(t *testing.T) {
	r, _, _ := clockedRegistry(t)
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "confirmation_token": "bogus"})
	var reply *mcp.ReplyError
	if !errors.As(err, &reply) {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{out, reply.Reply} {
		if !strings.HasPrefix(s, "{\n  \"confirmation_token\": ") {
			t.Errorf("confirmation_token is not the first key:\n%.200s", s)
		}
	}
}
