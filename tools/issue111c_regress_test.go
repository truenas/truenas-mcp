package tools

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// A preview that fails as a ReplyError carrying a blocked list must surface
// the structured blocked preview, not "Preview failed: ...".
func TestConfirmationRequiredShowsBlockedPreviewOnReplyError(t *testing.T) {
	r := NewRegistry(nil, nil)
	tool := r.tools["stop_app"]
	preview, err := json.Marshal(map[string]interface{}{
		"tool":     "stop_app",
		"blocked":  []string{"cannot stop app \"plex\" because of a test block"},
		"warnings": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool.Handler = func(_ context.Context, _ *truenas.Client, _ map[string]interface{}) (string, error) {
		return "", &mcp.ReplyError{
			Message: "cannot stop app \"plex\" because of a test block",
			Reply:   string(preview),
		}
	}
	// Clear Plan so confirmationRequired sees the handler's ReplyError
	// instead of planStopApp (which needs a client).
	tool.Plan = nil
	r.tools["stop_app"] = tool

	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex"})
	if err == nil {
		t.Fatalf("expected a blocked error, got %s", out)
	}
	msg := err.Error()
	if strings.Contains(msg, "Preview failed") {
		t.Errorf("blocked ReplyError was reported as a preview failure: %q", msg)
	}
	var reply *mcp.ReplyError
	if !errors.As(err, &reply) {
		t.Fatalf("want a ReplyError, got %T %v", err, err)
	}
	if !strings.Contains(reply.Reply, `"status": "blocked"`) || strings.Contains(reply.Reply, "confirmation_token") {
		t.Errorf("blocked reply: %s", reply.Reply)
	}
	if !strings.Contains(msg, "cannot proceed") || !strings.Contains(msg, "test block") {
		t.Errorf("error should say the call cannot proceed and include the blocked reason, got %q", msg)
	}
}

// Destructive tools that build a writePlan must issue and redeem tokens
// bound to that plan, not the legacy unbound issue/redeem path.
func TestDestructiveCallPathRequiresPlanBoundTokens(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "safety.go"))
	if err != nil {
		t.Fatal(err)
	}
	need := map[string]bool{"issueWithPlan": false, "redeemWithPlan": false}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "safety.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name != "callDestructive" && fn.Name.Name != "confirmationRequired" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := calleeName(call); need[name] || name == "issueWithPlan" || name == "redeemWithPlan" {
				need[name] = true
			}
			return true
		})
	}
	for name, found := range need {
		if !found {
			t.Errorf("callDestructive/confirmationRequired must call %s so a plan-backed destructive tool cannot get an unbound token", name)
		}
	}
}

// A blocked delete_boot_environment dry run lists reasons on the plan's
// blocked field, not only as BLOCKED: warnings.
func TestDeleteBootEnvironmentDryRunListsBlocked(t *testing.T) {
	bootEnvs := []interface{}{
		map[string]interface{}{"id": "running", "active": true, "activated": false, "keep": false},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": bootEnvs}, nil)
	r := NewRegistry(client, nil)
	out, err := r.CallTool(t.Context(), "delete_boot_environment", map[string]interface{}{
		"id": "running", "dry_run": true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var preview struct {
		Blocked  []string `json:"blocked"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("preview is not JSON: %v\n%s", err, out)
	}
	if len(preview.Blocked) == 0 {
		t.Fatalf("dry-run preview lists no blocked reasons:\n%s", out)
	}
	found := false
	for _, b := range preview.Blocked {
		if strings.Contains(b, "currently running") {
			found = true
		}
	}
	if !found {
		t.Errorf("blocked list %q should mention currently running", preview.Blocked)
	}
	if strings.Contains(out, "Delete boot environment") {
		t.Errorf("blocked preview should not plan a delete, got %s", out)
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a preview sent boot.environment.destroy: %v", calls)
	}
}
