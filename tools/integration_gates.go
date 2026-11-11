package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
)

// integrationWritesEnabled reports whether the live scratch write cycle is
// turned on. Only the exact value "1" enables it, matching the docs.
func integrationWritesEnabled() bool {
	return os.Getenv("TRUENAS_INTEGRATION_WRITES") == "1"
}

// integrationPoolName is the pool named by TRUENAS_INTEGRATION_POOL.
func integrationPoolName() string {
	return strings.TrimSpace(os.Getenv("TRUENAS_INTEGRATION_POOL"))
}

// integrationAppName is the installed app named by TRUENAS_INTEGRATION_APP.
// The live suite only stops or starts this app when the variable is set;
// it never auto-picks one for writes and never deletes an app.
func integrationAppName() string {
	return strings.TrimSpace(os.Getenv("TRUENAS_INTEGRATION_APP"))
}

// scratchWritePool returns the pool for the scratch write cycle, or a skip
// reason if TRUENAS_INTEGRATION_POOL is unset. It never falls back to the
// first pool on the system.
func scratchWritePool() (pool, skip string) {
	pool = integrationPoolName()
	if pool == "" {
		return "", "scratch write cycle requires TRUENAS_INTEGRATION_POOL to be set explicitly"
	}
	return pool, ""
}

// toolResultBody returns the structured tool result, including a ReplyError
// body when CallTool returned that error with an empty out string.
func toolResultBody(out string, err error) string {
	if out != "" {
		return out
	}
	var re *mcp.ReplyError
	if errors.As(err, &re) {
		return re.Reply
	}
	return ""
}

// dryRunResultError is nil when body is a JSON preview that reports
// dry_run: true and executed: false, the fields write-tool dry-runs set
// (see writePlan.dryRunResult and confirmationRequired).
func dryRunResultError(body string) error {
	if body == "" {
		return fmt.Errorf("empty dry-run result")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return fmt.Errorf("dry-run result is not JSON: %w", err)
	}
	if resp["dry_run"] != true {
		return fmt.Errorf("dry_run = %v, want true", resp["dry_run"])
	}
	if resp["executed"] != false {
		return fmt.Errorf("executed = %v, want false", resp["executed"])
	}
	return nil
}
