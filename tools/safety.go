package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
)

// Access classifies what a tool does to the TrueNAS system.
type Access int

const (
	// Unclassified is the zero value; registered tools must never use it.
	Unclassified Access = iota
	// Read tools only query state.
	Read
	// Write tools change state in a way that is additive or recoverable.
	Write
	// Destructive tools reboot the system, delete data or configuration,
	// cut off users, make one-way changes, interrupt running services, or
	// replace configuration in place. They execute only with a
	// confirmation_token from a preview of the same call
	// (tools/confirm_token.go).
	Destructive
)

func (a Access) String() string {
	switch a {
	case Read:
		return "read"
	case Write:
		return "write"
	case Destructive:
		return "destructive"
	}
	return "unclassified"
}

// Safety is a tool's entry in the central classification table.
type Safety struct {
	Access Access
	// Idempotent reports that repeating a call with the same arguments has
	// no further effect. Read tools are always idempotent.
	Idempotent bool
	// Overwrites marks a write tool that replaces existing state in place.
	// It advertises destructiveHint (MCP: "may perform destructive,
	// non-additive updates") without requiring a confirmation token.
	// Destructive tools always advertise destructiveHint. No tool uses it
	// now: update_app, the only one that did, is destructive.
	Overwrites bool
}

// toolSafety classifies every registered tool. A tool missing from this
// table fails TestEveryToolIsClassified and is treated as destructive at
// runtime. Any tool that deletes data or configuration, interrupts a
// running service, reboots, replaces configuration in place or makes a
// one-way change must be Destructive.
var toolSafety = map[string]Safety{
	// System
	"system_info":     {Access: Read},
	"system_health":   {Access: Read},
	"check_updates":   {Access: Read},
	"update_status":   {Access: Read},
	"download_update": {Access: Write},
	"apply_update":    {Access: Destructive},
	"system_reboot":   {Access: Destructive},

	// Boot environments
	"query_boot_environments":      {Access: Read},
	"get_current_boot_environment": {Access: Read},
	"delete_boot_environment":      {Access: Destructive, Idempotent: true},

	// Scrubs
	"query_scrub_schedules": {Access: Read},
	"get_scrub_status":      {Access: Read},
	"create_scrub_schedule": {Access: Write},
	"run_scrub":             {Access: Write},
	"delete_scrub_schedule": {Access: Destructive, Idempotent: true},

	// Directory services
	"get_directory_service_status": {Access: Read},
	"query_directory_services":     {Access: Read},
	"list_directory_certificates":  {Access: Read},
	"refresh_directory_cache":      {Access: Write, Idempotent: true},
	"configure_directory_service":  {Access: Destructive, Idempotent: true},
	"leave_directory_service":      {Access: Destructive, Idempotent: true},

	// Storage and shares
	"query_pools":               {Access: Read},
	"query_datasets":            {Access: Read},
	"query_snapshots":           {Access: Read},
	"query_shares":              {Access: Read},
	"query_vms":                 {Access: Read},
	"create_dataset":            {Access: Write},
	"create_smb_share":          {Access: Write},
	"create_nfs_share":          {Access: Write},
	"analyze_capacity":          {Access: Read},
	"get_pool_capacity_details": {Access: Read},

	// Alerts
	"list_alerts":   {Access: Read},
	"dismiss_alert": {Access: Write, Idempotent: true},
	"restore_alert": {Access: Write, Idempotent: true},

	// Metrics
	"get_system_metrics":  {Access: Read},
	"get_network_metrics": {Access: Read},
	"get_disk_metrics":    {Access: Read},
	"get_arc_metrics":     {Access: Read},
	"get_ups_metrics":     {Access: Read},

	// Apps
	"query_apps":              {Access: Read},
	"get_app_config":          {Access: Read},
	"search_app_catalog":      {Access: Read},
	"get_app_catalog_details": {Access: Read},
	"install_app":             {Access: Write},
	"update_app":              {Access: Destructive, Idempotent: true},
	"upgrade_app":             {Access: Destructive},
	"start_app":               {Access: Write, Idempotent: true},
	"stop_app":                {Access: Destructive, Idempotent: true},
	"delete_app":              {Access: Destructive, Idempotent: true},

	// Jobs and local task tracking
	"query_jobs": {Access: Read},
	"tasks_list": {Access: Read},
	"tasks_get":  {Access: Read},
}

// readOnlyDryRunTools are the write and destructive tools that --read-only
// still lists, for dry runs only: a call without dry_run=true is refused
// before anything runs. A tool belongs here only if its dry-run path queries
// the system and never changes it (TestReadOnlyDryRunMakesNoWrites).
var readOnlyDryRunTools = map[string]bool{
	"upgrade_app": true, // dry run: app.query and app.upgrade_summary only
}

// destructiveNote is appended to the description of every destructive tool.
const destructiveNote = " **DESTRUCTIVE: requires a confirmation_token.** Called without one, the server changes nothing and returns a preview plus a single-use confirmation_token, valid for 5 minutes and only for these exact arguments. Show the preview to the user, get their explicit approval, then call again with the same arguments plus confirmation_token."

// safetyOf returns a tool's classification, failing closed for tools that
// are missing from the table.
func safetyOf(name string) Safety {
	if s, ok := toolSafety[name]; ok && s.Access != Unclassified {
		return s
	}
	return Safety{Access: Destructive}
}

// Policy restricts which tools the server exposes.
type Policy struct {
	// ReadOnly hides and refuses every write and destructive tool, except
	// that readOnlyDryRunTools stay listed for dry runs only.
	ReadOnly bool
	// Allow, when non-empty, exposes only the named tools.
	Allow []string
	// Deny hides and refuses the named tools.
	Deny []string
}

// ParseToolList splits a comma-separated tool list, dropping blanks.
func ParseToolList(s string) []string {
	var names []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// SetPolicy validates p against the registered tools and applies it.
func (r *Registry) SetPolicy(p Policy) error {
	var unknown, readOnlyConflict []string
	allow := map[string]bool{}
	for _, n := range p.Allow {
		if _, ok := r.tools[n]; !ok {
			unknown = append(unknown, n)
		} else if p.ReadOnly && safetyOf(n).Access != Read && !readOnlyDryRunTools[n] {
			readOnlyConflict = append(readOnlyConflict, n)
		}
		allow[n] = true
	}
	deny := map[string]bool{}
	for _, n := range p.Deny {
		if _, ok := r.tools[n]; !ok {
			unknown = append(unknown, n)
		}
		deny[n] = true
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown tool name(s) in tool list: %s", strings.Join(unknown, ", "))
	}
	if len(readOnlyConflict) > 0 {
		sort.Strings(readOnlyConflict)
		return fmt.Errorf("--allow-tools lists write tool(s) %s, which --read-only disables", strings.Join(readOnlyConflict, ", "))
	}
	r.readOnly, r.allow, r.deny = p.ReadOnly, nil, deny
	if len(allow) > 0 {
		r.allow = allow
	}
	return nil
}

// checkPolicy reports why a registered tool may not be listed or called.
func (r *Registry) checkPolicy(name string) error {
	switch {
	case r.readOnly && safetyOf(name).Access != Read && !readOnlyDryRunTools[name]:
		return fmt.Errorf("tool %s is disabled: the server is running in read-only mode (--read-only / TRUENAS_MCP_READ_ONLY)", name)
	case r.allow != nil && !r.allow[name]:
		return fmt.Errorf("tool %s is disabled: it is not in --allow-tools / TRUENAS_MCP_ALLOW_TOOLS", name)
	case r.deny[name]:
		return fmt.Errorf("tool %s is disabled by --deny-tools / TRUENAS_MCP_DENY_TOOLS", name)
	}
	return nil
}

// validateSafetyBooleans rejects a dry_run or confirm value that is present
// but is not a JSON boolean (confirm is the pre-token flag that destructive
// tools still accept and ignore). A string such as "true" or a number must not
// fall through to a real write. The error names the argument, never echoes
// the value, and states that nothing was executed.
func validateSafetyBooleans(name string, args map[string]interface{}) error {
	for _, key := range []string{"dry_run", "confirm"} {
		v, present := args[key]
		if !present {
			continue
		}
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("invalid arguments for tool %s: %s must be a JSON boolean (true or false); nothing was executed", name, key)
		}
	}
	return nil
}

// checkReadOnlyDryRun refuses a call to a readOnlyDryRunTools tool in
// read-only mode unless it is an explicit dry run (dry_run is the boolean
// true). It runs before anything else, so a refused call executes nothing.
func (r *Registry) checkReadOnlyDryRun(name string, args map[string]interface{}) error {
	if !r.readOnly || !readOnlyDryRunTools[name] {
		return nil
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return nil
	}
	return fmt.Errorf("tool %s was NOT executed: the server is running in read-only mode (--read-only / TRUENAS_MCP_READ_ONLY), which allows only dry runs of %s; call it with dry_run=true to preview the change", name, name)
}

// readOnlyDryRunDefinition is how a readOnlyDryRunTools tool is listed in
// read-only mode: dry_run is required and must be true, confirmation_token
// is dropped (read-only mode issues no tokens),
// the description says only dry runs are allowed, and the annotations
// describe what the server will actually do (read only).
func readOnlyDryRunDefinition(def mcp.Tool) mcp.Tool {
	schema := make(map[string]interface{}, len(def.InputSchema))
	for k, v := range def.InputSchema {
		schema[k] = v
	}
	oldProps, _ := def.InputSchema["properties"].(map[string]interface{})
	props := make(map[string]interface{}, len(oldProps))
	for k, v := range oldProps {
		props[k] = v
	}
	delete(props, "confirm")
	delete(props, "confirmation_token")
	props["dry_run"] = map[string]interface{}{
		"type":        "boolean",
		"const":       true,
		"description": "Must be true: the server is in read-only mode and only previews the change without executing it.",
	}
	schema["properties"] = props
	required := []string{}
	switch req := def.InputSchema["required"].(type) {
	case []string:
		required = append(required, req...)
	case []interface{}:
		for _, v := range req {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	schema["required"] = append(required, "dry_run")
	def.InputSchema = schema
	def.Description = "**READ-ONLY MODE: only dry runs are allowed.** Call with dry_run=true to preview; any other call is refused and nothing is executed. " +
		strings.TrimSuffix(def.Description, destructiveNote)
	def.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	return def
}

// applySafety adds MCP annotations to every tool and the confirmation_token
// parameter to destructive tools, so the table above is the single source
// of truth.
func (r *Registry) applySafety() {
	for name, tool := range r.tools {
		s := safetyOf(name)
		tool.Definition.Annotations = &mcp.ToolAnnotations{
			ReadOnlyHint:    s.Access == Read,
			DestructiveHint: s.Access == Destructive || s.Overwrites,
			IdempotentHint:  s.Access == Read || s.Idempotent,
			OpenWorldHint:   false,
		}
		if s.Access == Destructive {
			props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
			if props == nil {
				props = map[string]interface{}{}
				tool.Definition.InputSchema["properties"] = props
			}
			props["confirmation_token"] = map[string]interface{}{
				"type":        "string",
				"description": "Required to execute. Omit it first: the server returns a preview and a confirmation_token and changes nothing. After the user has explicitly approved the preview, repeat the call with exactly the same arguments plus this token. Single use, valid for 5 minutes.",
			}
			tool.Definition.Description += destructiveNote
		}
		r.tools[name] = tool
	}
}

// hasParam reports whether a tool's input schema declares the parameter.
func hasParam(tool Tool, name string) bool {
	props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
	_, ok := props[name]
	return ok
}

// callDestructive runs a destructive tool. dry_run=true (on a tool that
// declares it) only previews. A valid confirmation_token for this tool and
// these exact arguments executes the call, once. Anything else runs the
// preview and returns it with a fresh token, or with an error when the
// preview says the operation cannot proceed. legacyConfirm reports that the
// caller sent the old confirm=true.
func (r *Registry) callDestructive(ctx context.Context, name string, tool Tool, args map[string]interface{}, legacyConfirm bool) (string, error) {
	if dryRun, _ := args["dry_run"].(bool); dryRun && hasParam(tool, "dry_run") {
		return tool.Handler(ctx, r.client, withoutKeys(args, "confirmation_token"))
	}
	argsHash, err := confirmationArgsHash(args)
	if err != nil {
		return "", fmt.Errorf("%s was NOT executed: its arguments could not be encoded; nothing was executed", name)
	}
	var planHash [32]byte
	var plan *writePlan
	if tool.Plan != nil {
		plan, err = tool.Plan(ctx, r.client, withoutKeys(args, "confirmation_token", "dry_run"))
		if err != nil {
			return "", err
		}
		planHash, err = plan.planHash()
		if err != nil {
			return "", fmt.Errorf("%s was NOT executed: its plan could not be encoded; nothing was executed", name)
		}
	}
	var rejected error
	if v, sent := args["confirmation_token"]; sent {
		tok, _ := v.(string)
		if tool.Plan != nil {
			rejected = r.confirmTokens.redeemWithPlan(tok, name, argsHash, planHash)
		} else {
			rejected = r.confirmTokens.redeem(tok, name, argsHash)
		}
		if rejected == nil {
			if err := plan.blockedReply(); err != nil {
				return "", err
			}
			return tool.Handler(ctx, r.client, withoutKeys(args, "confirmation_token", "dry_run"))
		}
	}
	return r.confirmationRequired(ctx, name, tool, withoutKeys(args, "confirmation_token", "dry_run"), argsHash, planHash, rejected, legacyConfirm)
}

// confirmationRequired runs the tool's dry-run preview and returns it with a
// new confirmation token for these arguments. A preview that fails, or that
// reports the operation cannot proceed, is an error and issues no token. When
// a presented token was rejected, the reply (with the fresh preview and
// token) is an error that gives the reason.
func (r *Registry) confirmationRequired(ctx context.Context, name string, tool Tool, args map[string]interface{}, argsHash, planHash [32]byte, rejected error, legacyConfirm bool) (string, error) {
	reply := map[string]interface{}{
		"status":   "confirmation_required",
		"tool":     name,
		"executed": false,
	}
	rejectedNote := ""
	if rejected != nil {
		rejectedNote = rejected.Error() + ". "
	}
	if hasParam(tool, "dry_run") {
		preview := make(map[string]interface{}, len(args)+1)
		for k, v := range args {
			preview[k] = v
		}
		preview["dry_run"] = true
		out, err := tool.Handler(ctx, r.client, preview)
		if err != nil {
			var re *mcp.ReplyError
			// A blocked plan returns a ReplyError whose body is the
			// structured preview. Surface that preview (status blocked,
			// no token) instead of wrapping it as "Preview failed".
			if errors.As(err, &re) && len(blockedReasons(re.Reply)) > 0 {
				out = re.Reply
			} else {
				return "", fmt.Errorf("%s%s was NOT executed. Preview failed: %w. Resolve this before confirming; "+
					"the user must see a successful preview and explicitly approve it first", rejectedNote, name, err)
			}
		}
		// The preview is a dry run: say so explicitly, next to executed.
		reply["dry_run"] = true
		if json.Valid([]byte(out)) {
			reply["preview"] = json.RawMessage(out)
		} else {
			reply["preview"] = out
		}
		if reasons := blockedReasons(out); len(reasons) > 0 {
			msg := fmt.Sprintf("%s%s cannot proceed: %s; nothing was executed and no confirmation token was issued. "+
				"Do not ask the user to confirm this call; resolve the problem shown in the preview first", rejectedNote, name, strings.Join(reasons, "; "))
			reply["status"] = "blocked"
			reply["error"] = msg
			body, err := marshalJSON(reply)
			if err != nil {
				return "", errors.New(msg)
			}
			return "", &mcp.ReplyError{Message: msg, Reply: body}
		}
	}
	var tok string
	var err error
	if tool.Plan != nil {
		tok, err = r.confirmTokens.issueWithPlan(name, argsHash, planHash)
	} else {
		tok, err = r.confirmTokens.issue(name, argsHash)
	}
	if err != nil {
		return "", fmt.Errorf("%s%s was NOT executed: %w; nothing was executed", rejectedNote, name, err)
	}
	reply["confirmation_token"] = tok
	reply["expires_in_seconds"] = int(confirmationTokenTTL / time.Second)
	msg := fmt.Sprintf("%s is a destructive operation and was NOT executed. Show the user the preview, ask them to explicitly approve it, "+
		"and only then call %s again with exactly the same arguments plus confirmation_token. The token is single use, "+
		"expires in %d minutes and works only for these arguments; any change needs a new preview.", name, name, int(confirmationTokenTTL/time.Minute))
	if legacyConfirm {
		msg = "confirm=true no longer executes destructive tools: they need the confirmation_token returned with a preview. " + msg
	}
	reply["message"] = msg
	body, err := marshalJSON(reply)
	if err != nil {
		return "", err
	}
	if rejected != nil {
		errMsg := fmt.Sprintf("%s: %s; nothing was executed. A fresh preview and confirmation_token are attached: show the preview to the user "+
			"and only after they approve it call %s again with the same arguments plus the new confirmation_token", name, rejected, name)
		reply["error"] = errMsg
		if withErr, err := marshalJSON(reply); err == nil {
			body = withErr
		}
		return "", &mcp.ReplyError{Message: errMsg, Reply: body}
	}
	return body, nil
}

// blockedReasons returns the reasons a dry-run preview says the operation
// cannot proceed. It prefers the plan's blocked list, and also honours
// warnings that start with "BLOCKED:" (as in delete_boot_environment on
// the running boot environment) for tools not yet on the plan.
func blockedReasons(preview string) []string {
	var p struct {
		Blocked  []interface{} `json:"blocked"`
		Warnings []interface{} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(preview), &p); err != nil {
		return nil
	}
	var reasons []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		reasons = append(reasons, s)
	}
	for _, b := range p.Blocked {
		if s, ok := b.(string); ok {
			add(strings.TrimSpace(s))
		}
	}
	for _, w := range p.Warnings {
		if s, ok := w.(string); ok && strings.HasPrefix(s, "BLOCKED:") {
			add(strings.TrimSpace(strings.TrimPrefix(s, "BLOCKED:")))
		}
	}
	return reasons
}
