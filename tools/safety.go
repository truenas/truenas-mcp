package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	// cut off users, make one-way changes, or interrupt running services.
	// They require confirm=true to execute.
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
	// non-additive updates") without requiring confirm. Destructive tools
	// always advertise destructiveHint.
	Overwrites bool
	// RevealsSecret marks a tool whose successful result deliberately
	// carries secret material (an encryption key) to the client. Like a
	// destructive tool it requires confirm=true, and the stdio handler
	// exempts its ExportedKeyField from output redaction. Only the tool's
	// own result is exempt; logs, errors and every other tool stay redacted.
	RevealsSecret bool
}

// toolSafety classifies every registered tool. A tool missing from this
// table fails TestEveryToolIsClassified and is treated as destructive at
// runtime.
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
	"export_dataset_key":        {Access: Write, Idempotent: true, RevealsSecret: true},
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
	"update_app":              {Access: Write, Idempotent: true, Overwrites: true},
	"upgrade_app":             {Access: Destructive},
	"start_app":               {Access: Write, Idempotent: true},
	"stop_app":                {Access: Destructive, Idempotent: true},
	"delete_app":              {Access: Destructive, Idempotent: true},

	// Jobs and local task tracking
	"query_jobs": {Access: Read},
	"tasks_list": {Access: Read},
	"tasks_get":  {Access: Read},
}

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
	// ReadOnly hides and refuses every write and destructive tool.
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
		} else if p.ReadOnly && safetyOf(n).Access != Read {
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

// RevealsSecret reports whether a tool deliberately returns secret material
// in its result (see Safety.RevealsSecret); the output redactor exempts
// ExportedKeyField for exactly these tools.
func (r *Registry) RevealsSecret(name string) bool {
	_, registered := r.tools[name]
	return registered && safetyOf(name).RevealsSecret
}

// checkPolicy reports why a registered tool may not be listed or called.
func (r *Registry) checkPolicy(name string) error {
	switch {
	case r.readOnly && safetyOf(name).Access != Read:
		return fmt.Errorf("tool %s is disabled: the server is running in read-only mode (--read-only / TRUENAS_MCP_READ_ONLY)", name)
	case r.allow != nil && !r.allow[name]:
		return fmt.Errorf("tool %s is disabled: it is not in --allow-tools / TRUENAS_MCP_ALLOW_TOOLS", name)
	case r.deny[name]:
		return fmt.Errorf("tool %s is disabled by --deny-tools / TRUENAS_MCP_DENY_TOOLS", name)
	}
	return nil
}

// applySafety adds MCP annotations to every tool and the confirm parameter
// to destructive tools, so the table above is the single source of truth.
func (r *Registry) applySafety() {
	for name, tool := range r.tools {
		s := safetyOf(name)
		tool.Definition.Annotations = &mcp.ToolAnnotations{
			ReadOnlyHint:    s.Access == Read,
			DestructiveHint: s.Access == Destructive || s.Overwrites,
			IdempotentHint:  s.Access == Read || s.Idempotent,
			OpenWorldHint:   false,
		}
		if s.Access == Destructive || s.RevealsSecret {
			props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
			if props == nil {
				props = map[string]interface{}{}
				tool.Definition.InputSchema["properties"] = props
			}
			confirmDesc := "Required to execute: set to true only after the user has explicitly approved this destructive operation. Without it the server returns a preview and changes nothing."
			descSuffix := " **DESTRUCTIVE: requires confirm=true.** Without confirm the server only returns a preview; show it to the user and get their explicit approval before calling again with confirm=true."
			if s.Access != Destructive {
				confirmDesc = "Required to execute: set to true only after the user has explicitly approved revealing this secret in the conversation. Without it the server returns a preview and reveals nothing."
				descSuffix = " **REVEALS A SECRET: requires confirm=true.** Without confirm the server only returns a preview; show it to the user and get their explicit approval before calling again with confirm=true."
			}
			props["confirm"] = map[string]interface{}{
				"type":        "boolean",
				"description": confirmDesc,
				"default":     false,
			}
			tool.Definition.Description += descSuffix
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

// requiresConfirmation reports whether a destructive call must be held for
// explicit approval: confirm is not true and this is not a dry run.
// dry_run only counts for tools that declare it; a handler without dry-run
// support would otherwise execute for real.
func requiresConfirmation(tool Tool, args map[string]interface{}) bool {
	if confirm, _ := args["confirm"].(bool); confirm {
		return false
	}
	dryRun, _ := args["dry_run"].(bool)
	return !(dryRun && hasParam(tool, "dry_run"))
}

// confirmationRequired builds the non-error reply for a destructive call
// made without confirm=true, including the tool's dry-run preview.
func (r *Registry) confirmationRequired(name string, tool Tool, args map[string]interface{}) (string, error) {
	reply := map[string]interface{}{
		"status":   "confirmation_required",
		"tool":     name,
		"executed": false,
		"message": fmt.Sprintf("%s is a destructive operation and was NOT executed. Show the user what it will do, "+
			"ask them to explicitly approve it, and only then call %s again with the same arguments plus confirm=true.", name, name),
	}
	if safetyOf(name).RevealsSecret {
		reply["message"] = fmt.Sprintf("%s reveals a secret and was NOT executed: nothing has been revealed. Tell the user the secret will be shown in this conversation "+
			"(the model's context and any client logs), ask them to explicitly approve it, and only then call %s again with the same arguments plus confirm=true.", name, name)
	}
	if hasParam(tool, "dry_run") {
		preview := make(map[string]interface{}, len(args)+1)
		for k, v := range args {
			preview[k] = v
		}
		preview["dry_run"] = true
		out, err := tool.Handler(r.client, preview)
		if err != nil {
			return "", fmt.Errorf("%s was NOT executed. Preview failed: %w. Resolve this before confirming; "+
				"the user must see a successful preview and explicitly approve it first", name, err)
		}
		if json.Valid([]byte(out)) {
			reply["preview"] = json.RawMessage(out)
		} else {
			reply["preview"] = out
		}
	}
	return marshalJSON(reply)
}
