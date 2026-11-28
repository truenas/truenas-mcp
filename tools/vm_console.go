package tools

// VM serial console tools (open_vm_console, read_vm_console,
// send_vm_console_keys, close_vm_console).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	authGenerateTokenMethod = "auth.generate_token"
)

// vmConsoleUUIDMinVersion is the first TrueNAS release where webshell_app.py
// attaches virsh console by VM UUID and enforces VM_WRITE in addition to
// web_shell (TrueNAS 26.0 / master). Earlier supported releases (25.10
// stable/goldeye) attach by "<id>_<name>" and check web_shell only.
var vmConsoleUUIDMinVersion = truenas.MustParseVersion("26.0")

func (r *Registry) registerVMConsoleTools() {
	openProps := vmSelectorSchemaProps()
	openProps["cols"] = map[string]interface{}{
		"type":        "integer",
		"minimum":     minVMConsoleCols,
		"maximum":     maxVMConsoleCols,
		"default":     defaultVMConsoleCols,
		"description": fmt.Sprintf("Optional: Terminal width in columns (%d-%d, default %d)", minVMConsoleCols, maxVMConsoleCols, defaultVMConsoleCols),
	}
	openProps["rows"] = map[string]interface{}{
		"type":        "integer",
		"minimum":     minVMConsoleRows,
		"maximum":     maxVMConsoleRows,
		"default":     defaultVMConsoleRows,
		"description": fmt.Sprintf("Optional: Terminal height in rows (%d-%d, default %d)", minVMConsoleRows, maxVMConsoleRows, defaultVMConsoleRows),
	}

	r.tools["open_vm_console"] = Tool{
		Definition: mcp.Tool{
			Name: "open_vm_console",
			Description: "Open an interactive serial console session to a RUNNING virtual machine over TrueNAS's /websocket/shell endpoint and return an opaque session_id plus the initial rendered terminal screen. " +
				"Only VMs explicitly listed in --allow-vm-console (or TRUENAS_MCP_ALLOW_VM_CONSOLE) are permitted; all VMs are refused when the allowlist is unset. " +
				"Refuses VMs that are not RUNNING, refuses unknown or unsupported TrueNAS versions, enforces a cap on concurrent sessions, and closes idle sessions automatically after 10 minutes or on server shutdown. " +
				"Requires web_shell privilege on TrueNAS 25.10 (and VM_WRITE on TrueNAS 26.0+). " +
				"WARNING: Keystrokes sent to a serial console can execute arbitrary actions inside the guest. Use dry_run=true to preview first.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": openProps,
			},
		},
		Plan:    r.planOpenVMConsole,
		Execute: r.executeOpenVMConsole,
		Handler: r.handleOpenVMConsole,
	}

	r.tools["read_vm_console"] = Tool{
		Definition: mcp.Tool{
			Name: "read_vm_console",
			Description: "Read the current rendered VT100/xterm screen text and cursor position of an open VM serial console session. " +
				"Optionally waits (without busy polling) until wait_for (substring) or wait_for_regex (RE2 regex) appears on the rendered screen, or until timeout_seconds elapses.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"description": "Opaque session_id returned by open_vm_console",
					},
					"wait_for": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"maxLength":   vmConsoleMaxWaitPatternBytes,
						"description": "Optional: Substring to wait for on the rendered screen (mutually exclusive with wait_for_regex)",
					},
					"wait_for_regex": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"maxLength":   vmConsoleMaxWaitPatternBytes,
						"description": "Optional: Go RE2 regular expression to wait for on the rendered screen (mutually exclusive with wait_for)",
					},
					"timeout_seconds": map[string]interface{}{
						"type":        "number",
						"minimum":     0,
						"maximum":     int(maxVMConsoleWaitTimeout / time.Second),
						"description": fmt.Sprintf("Optional: Maximum seconds to wait when wait_for or wait_for_regex is set (0-%d, default %d when waiting, 0 otherwise)", int(maxVMConsoleWaitTimeout/time.Second), int(defaultVMConsoleWaitTimeout/time.Second)),
					},
				},
				"required": []string{"session_id"},
			},
		},
		Handler: r.handleReadVMConsole,
	}

	r.tools["send_vm_console_keys"] = Tool{
		Definition: mcp.Tool{
			Name: "send_vm_console_keys",
			Description: "Send literal text and/or named keys to an open VM serial console session. " +
				"Literal text is sent first as UTF-8 bytes, followed by any named keys (" + supportedVMConsoleKeysHint + "). " +
				"Payload is capped at 4096 bytes and 256 keys per call. " +
				"Input is NEVER echoed in responses or written to server logs; set sensitive=true for passwords or passphrases to also omit byte and key counts from the response. " +
				"Note: Ctrl+] detaches virsh console on TrueNAS; use close_vm_console to end a session cleanly.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"description": "Opaque session_id returned by open_vm_console",
					},
					"text": map[string]interface{}{
						"type":        "string",
						"maxLength":   vmConsoleMaxPayloadBytes,
						"description": "Optional: Literal UTF-8 text to type into the serial console (never echoed or logged)",
					},
					"keys": map[string]interface{}{
						"type":     "array",
						"maxItems": vmConsoleMaxKeys,
						"items": map[string]interface{}{
							"type": "string",
						},
						"description": "Optional: Named keys to send after text (e.g. Enter, Tab, Shift+Tab, Backspace, Esc, Space, Up/Down/Left/Right, Home/End, Insert/Delete, PageUp/PageDown, F1-F12, Ctrl+C, Alt+F2)",
					},
					"sensitive": map[string]interface{}{
						"type":        "boolean",
						"default":     false,
						"description": "Optional: Mark input as sensitive (e.g. passwords); input is never echoed or logged regardless, and sensitive=true also omits byte and key counts from the response",
					},
				},
				"required": []string{"session_id"},
			},
		},
		Handler: r.handleSendVMConsoleKeys,
	}

	r.tools["close_vm_console"] = Tool{
		Definition: mcp.Tool{
			Name:        "close_vm_console",
			Description: "Close an open VM serial console session and release its underlying WebSocket connection and terminal buffer.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"session_id": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"description": "Opaque session_id returned by open_vm_console",
					},
				},
				"required": []string{"session_id"},
			},
		},
		Handler: r.handleCloseVMConsole,
	}
}

type authMePrivilege struct {
	Present  bool
	WebShell bool
	Roles    []string
	Username string
}

func fetchAuthMePrivilege(ctx context.Context, client *truenas.Client) (authMePrivilege, error) {
	raw, err := client.Call(ctx, "auth.me")
	if err != nil {
		return authMePrivilege{}, fmt.Errorf("failed to verify account privileges via auth.me: %w", err)
	}
	var resp struct {
		Username  string `json:"pw_name"`
		Privilege *struct {
			WebShell bool     `json:"web_shell"`
			Roles    []string `json:"roles"`
		} `json:"privilege"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return authMePrivilege{}, fmt.Errorf("failed to parse auth.me response: %w", err)
	}
	out := authMePrivilege{Username: resp.Username}
	if resp.Privilege != nil {
		out.Present = true
		out.WebShell = resp.Privilege.WebShell
		out.Roles = resp.Privilege.Roles
	}
	return out, nil
}

func hasRole(roles []string, target string) bool {
	for _, r := range roles {
		if r == target {
			return true
		}
	}
	return false
}

func vmConsoleDomainForVersion(ver truenas.Version, vm *resolvedVM) (string, error) {
	if ver.IsZero() || ver.Less(truenas.MinSupported) {
		return "", fmt.Errorf("cannot open VM serial console: TrueNAS version is unknown or unsupported (%s); serial console requires TrueNAS %s or later", vmVersionLabel(ver), truenas.MinSupported)
	}
	if ver.AtLeast(vmConsoleUUIDMinVersion) {
		if strings.TrimSpace(vm.UUID) == "" {
			return "", fmt.Errorf("cannot open serial console for VM %q (id %d): VM UUID is empty (TrueNAS %s attaches virsh console by VM UUID)", vm.Name, vm.ID, ver)
		}
		return vm.UUID, nil
	}
	return fmt.Sprintf("%d_%s", vm.ID, vm.Name), nil
}

func (r *Registry) planOpenVMConsole(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	cols, _, err := vmIntArg(args, "cols", defaultVMConsoleCols)
	if err != nil {
		return nil, err
	}
	if cols < minVMConsoleCols || cols > maxVMConsoleCols {
		return nil, fmt.Errorf("cols must be between %d and %d", minVMConsoleCols, maxVMConsoleCols)
	}
	rows, _, err := vmIntArg(args, "rows", defaultVMConsoleRows)
	if err != nil {
		return nil, err
	}
	if rows < minVMConsoleRows || rows > maxVMConsoleRows {
		return nil, fmt.Errorf("rows must be between %d and %d", minVMConsoleRows, maxVMConsoleRows)
	}

	ver, err := client.ServerVersion(ctx)
	if err != nil || ver.IsZero() || ver.Less(truenas.MinSupported) {
		detail := vmVersionLabel(ver)
		if err != nil {
			detail = fmt.Sprintf("version lookup failed: %v", err)
		}
		return nil, fmt.Errorf("cannot open VM serial console: TrueNAS version is unknown or unsupported (%s); serial console requires TrueNAS %s or later so libvirt domain format and required roles can be verified", detail, truenas.MinSupported)
	}

	vm, err := lookupVMWithStatus(ctx, client, args)
	if err != nil {
		return nil, err
	}

	if err := r.vmConsoles.checkAllowed(vm.ID, vm.Name); err != nil {
		return nil, err
	}

	var blocked, warnings []string
	if vm.State != "RUNNING" {
		blocked = append(blocked, fmt.Sprintf("cannot open serial console for VM %q (id %d): VM state is %s (must be RUNNING); start the VM with start_vm first", vm.Name, vm.ID, vm.State))
	}

	domain, err := vmConsoleDomainForVersion(ver, vm)
	if err != nil {
		blocked = append(blocked, err.Error())
	}

	if err := r.vmConsoles.checkCapacity(vm.ID, vm.Name); err != nil {
		blocked = append(blocked, err.Error())
	}

	priv, err := fetchAuthMePrivilege(ctx, client)
	if err != nil {
		return nil, err
	}
	if priv.Present {
		if !priv.WebShell {
			blocked = append(blocked, "TrueNAS account lacks the web_shell privilege required for /websocket/shell")
		}
		if ver.AtLeast(vmConsoleUUIDMinVersion) && !hasRole(priv.Roles, "VM_WRITE") && !hasRole(priv.Roles, "FULL_ADMIN") {
			blocked = append(blocked, fmt.Sprintf("TrueNAS account lacks the VM_WRITE role required to open a VM serial console on TrueNAS %s", ver))
		}
	}

	warnings = append(warnings,
		"Keystrokes sent to a serial console can execute arbitrary actions inside the guest OS.",
		fmt.Sprintf("Session closes automatically after %s of client inactivity or on server shutdown; use close_vm_console when finished.", r.vmConsoles.idleTimeout),
	)
	if !ver.AtLeast(vmConsoleUUIDMinVersion) {
		warnings = append(warnings, fmt.Sprintf("TrueNAS %s attaches virsh console using domain %q; non-root accounts without sudo ALL may be refused by libvirt.", ver, domain))
	}

	current := vm.currentState()
	current["truenas_version"] = ver.String()
	current["libvirt_domain"] = domain
	current["cols"] = cols
	current["rows"] = rows
	current["idle_timeout_seconds"] = int(r.vmConsoles.idleTimeout / time.Second)
	current["max_concurrent_sessions"] = r.vmConsoles.maxSessions

	headline := fmt.Sprintf("Open serial console for VM %q (id %d, %dx%d)", vm.Name, vm.ID, cols, rows)
	return &writePlan{
		Tool:         "open_vm_console",
		Method:       authGenerateTokenMethod,
		CallParams:   []interface{}{60, map[string]interface{}{}, true, true},
		ParamNames:   []string{"ttl", "attrs", "match_origin", "single_use"},
		Description:  headline,
		Operation:    "open_serial_console",
		CurrentState: current,
		Warnings:     warnings,
		Blocked:      blocked,
		TargetName:   vm.Name,
		Bind: map[string]interface{}{
			"vm_id":           vm.ID,
			"name":            vm.Name,
			"uuid":            vm.UUID,
			"status":          vm.statusBind(),
			"truenas_version": ver.String(),
			"libvirt_domain":  domain,
			"cols":            cols,
			"rows":            rows,
		},
		ConfirmSummary: newConfirmSummary(
			headline,
			confirmFact("VM", fmt.Sprintf("%s (id %d)", vm.Name, vm.ID)),
			confirmFact("current state", vm.State),
			confirmFact("TrueNAS version", ver.String()),
			confirmFact("libvirt domain", domain),
			confirmFact("terminal size", fmt.Sprintf("%dx%d", cols, rows)),
			confirmFact("effect", "attaches interactive serial console (keystrokes can run commands in guest)"),
		),
		Result: map[string]interface{}{
			"vm_id":           vm.ID,
			"vm_name":         vm.Name,
			"libvirt_domain":  domain,
			"truenas_version": ver,
			"cols":            cols,
			"rows":            rows,
		},
	}, nil
}

func (r *Registry) handleOpenVMConsole(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return handlePlanned(ctx, client, args, r.planOpenVMConsole, r.executeOpenVMConsole)
}

func (r *Registry) executeOpenVMConsole(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	vmID, _ := plan.Result["vm_id"].(int)
	vmName, _ := plan.Result["vm_name"].(string)
	domain, _ := plan.Result["libvirt_domain"].(string)
	ver, _ := plan.Result["truenas_version"].(truenas.Version)
	cols, _ := plan.Result["cols"].(int)
	rows, _ := plan.Result["rows"].(int)

	// Re-check allowlist and live VM state right before opening in case state
	// changed between plan/confirmation and execution.
	if err := r.vmConsoles.checkAllowed(vmID, vmName); err != nil {
		return "", err
	}
	statusObj, err := fetchVMStatus(ctx, client, vmID)
	if err != nil {
		return "", fmt.Errorf("failed to re-check VM %q (id %d) status: %w", vmName, vmID, err)
	}
	state, _ := statusObj["state"].(string)
	state = strings.ToUpper(state)
	if state != "RUNNING" {
		return "", fmt.Errorf("cannot open serial console for VM %q (id %d): VM state is now %s (must be RUNNING)", vmName, vmID, state)
	}

	if err := r.vmConsoles.reserveSlot(vmID, vmName); err != nil {
		return "", err
	}
	reserved := true
	defer func() {
		if reserved {
			r.vmConsoles.releaseReservation(vmID)
		}
	}()

	shell, err := r.vmConsoles.opener(ctx, client, vmID)
	if err != nil {
		return "", formatOpenVMShellError(ver, vmName, vmID, err)
	}

	if shellID := shell.ID(); shellID != "" {
		if _, rerr := client.Call(ctx, "core.resize_shell", shellID, cols, rows); rerr != nil {
			log.Printf("Warning: core.resize_shell failed for VM %d (%dx%d): %v", vmID, cols, rows, rerr)
		}
	}

	sessionID, err := generateVMConsoleSessionID()
	if err != nil {
		_ = shell.Close()
		return "", err
	}

	session := newVMConsoleSession(sessionID, vmID, vmName, domain, cols, rows, shell, r.vmConsoles.unregisterSession)
	if err := r.vmConsoles.registerSession(session); err != nil {
		reserved = false
		session.Close("manager closed")
		return "", err
	}
	reserved = false

	// Read back initial terminal state and verify virsh console did not exit
	// immediately (e.g. another console already attached, or libvirt permission error).
	snap := session.waitInitialSettle(ctx, r.vmConsoles.settleTimeout)
	if !snap.Connected {
		session.Close("remote closed during open")
		detail := strings.TrimSpace(snap.Screen)
		if detail != "" {
			return "", fmt.Errorf("VM serial console for %q (id %d) disconnected immediately after opening: %s", vmName, vmID, detail)
		}
		return "", fmt.Errorf("VM serial console for %q (id %d) disconnected immediately after opening (another console session may already be attached or virsh failed to attach to %s)", vmName, vmID, domain)
	}

	out := map[string]interface{}{
		"status":               "opened",
		"session_id":           snap.SessionID,
		"vm_id":                snap.VMID,
		"vm_name":              snap.VMName,
		"libvirt_domain":       domain,
		"truenas_version":      ver.String(),
		"cols":                 snap.Cols,
		"rows":                 snap.Rows,
		"cursor":               map[string]interface{}{"row": snap.CursorRow, "col": snap.CursorCol, "hidden": snap.CursorHidden},
		"alt_screen":           snap.AltScreen,
		"screen":               snap.Screen,
		"truncated":            snap.Truncated,
		"connected":            snap.Connected,
		"idle_timeout_seconds": int(r.vmConsoles.idleTimeout / time.Second),
		"note":                 "Use read_vm_console to read updates, send_vm_console_keys to type input, and close_vm_console when done.",
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func formatOpenVMShellError(ver truenas.Version, vmName string, vmID int, err error) error {
	var authErr *truenas.ShellAuthError
	if errors.As(err, &authErr) {
		if ver.AtLeast(vmConsoleUUIDMinVersion) {
			return fmt.Errorf("failed to open serial console for VM %q (id %d): %w (TrueNAS %s requires web_shell privilege and the VM_WRITE role)", vmName, vmID, authErr, ver)
		}
		return fmt.Errorf("failed to open serial console for VM %q (id %d): %w (TrueNAS %s requires the web_shell privilege and matching client origin IP)", vmName, vmID, authErr, ver)
	}
	return fmt.Errorf("failed to open serial console for VM %q (id %d): %w", vmName, vmID, err)
}

func (r *Registry) handleReadVMConsole(ctx context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
	sessionID, _ := args["session_id"].(string)
	waitFor, err := optionalStringArg(args, "wait_for")
	if err != nil {
		return "", err
	}
	waitRegexStr, err := optionalStringArg(args, "wait_for_regex")
	if err != nil {
		return "", err
	}
	if waitFor != "" && waitRegexStr != "" {
		return "", fmt.Errorf("wait_for and wait_for_regex are mutually exclusive")
	}
	if len(waitFor) > vmConsoleMaxWaitPatternBytes {
		return "", fmt.Errorf("wait_for exceeds maximum length of %d bytes", vmConsoleMaxWaitPatternBytes)
	}
	if len(waitRegexStr) > vmConsoleMaxWaitPatternBytes {
		return "", fmt.Errorf("wait_for_regex exceeds maximum length of %d bytes", vmConsoleMaxWaitPatternBytes)
	}

	var waitRe *regexp.Regexp
	if waitRegexStr != "" {
		waitRe, err = regexp.Compile(waitRegexStr)
		if err != nil {
			return "", fmt.Errorf("invalid wait_for_regex: %w", err)
		}
	}

	waiting := waitFor != "" || waitRe != nil
	timeout := time.Duration(0)
	if waiting {
		timeout = defaultVMConsoleWaitTimeout
	}
	if rawTimeout, exists := args["timeout_seconds"]; exists && rawTimeout != nil {
		var f float64
		switch v := rawTimeout.(type) {
		case float64:
			f = v
		case int:
			f = float64(v)
		default:
			return "", fmt.Errorf("timeout_seconds must be a number")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("timeout_seconds must be a number")
		}
		if f < 0 || f > float64(maxVMConsoleWaitTimeout/time.Second) {
			return "", fmt.Errorf("timeout_seconds must be between 0 and %d", int(maxVMConsoleWaitTimeout/time.Second))
		}
		timeout = time.Duration(f * float64(time.Second))
	}

	session, err := r.vmConsoles.getSession(sessionID)
	if err != nil {
		return "", err
	}

	snap, err := session.readScreen(ctx, waitFor, waitRe, timeout)
	if err != nil {
		return "", err
	}

	out := map[string]interface{}{
		"session_id": snap.SessionID,
		"vm_id":      snap.VMID,
		"vm_name":    snap.VMName,
		"cols":       snap.Cols,
		"rows":       snap.Rows,
		"cursor":     map[string]interface{}{"row": snap.CursorRow, "col": snap.CursorCol, "hidden": snap.CursorHidden},
		"alt_screen": snap.AltScreen,
		"screen":     snap.Screen,
		"truncated":  snap.Truncated,
		"connected":  snap.Connected,
	}
	if !snap.Connected && snap.CloseReason != "" {
		out["close_reason"] = snap.CloseReason
	}
	if waiting {
		out["matched"] = snap.Matched
		out["timed_out"] = snap.TimedOut
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *Registry) handleSendVMConsoleKeys(_ context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
	sessionID, _ := args["session_id"].(string)
	text, err := optionalStringArg(args, "text")
	if err != nil {
		return "", err
	}
	keys, err := parseVMConsoleKeysArg(args)
	if err != nil {
		return "", err
	}
	sensitive := false
	if rawSens, exists := args["sensitive"]; exists && rawSens != nil {
		b, ok := rawSens.(bool)
		if !ok {
			return "", fmt.Errorf("sensitive must be a boolean")
		}
		sensitive = b
	}

	session, err := r.vmConsoles.getSession(sessionID)
	if err != nil {
		return "", err
	}

	bytesSent, keysSent, err := session.sendInput(text, keys)
	if err != nil {
		return "", err
	}

	out := map[string]interface{}{
		"status":     "sent",
		"session_id": session.id,
		"vm_id":      session.vmID,
		"vm_name":    session.vmName,
		"sensitive":  sensitive,
	}
	if sensitive {
		out["note"] = "Sensitive input sent; payload and length omitted and never logged. Call read_vm_console to observe the guest screen."
	} else {
		out["bytes_sent"] = bytesSent
		out["keys_sent"] = keysSent
		out["note"] = "Input is never echoed or logged. Call read_vm_console to observe the guest screen."
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *Registry) handleCloseVMConsole(ctx context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
	sessionID, _ := args["session_id"].(string)
	session, err := r.vmConsoles.closeSession(ctx, sessionID, "closed by client")
	if err != nil {
		return "", err
	}
	out := map[string]interface{}{
		"status":     "closed",
		"session_id": session.id,
		"vm_id":      session.vmID,
		"vm_name":    session.vmName,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func fetchVMStatus(ctx context.Context, client *truenas.Client, vmID int) (map[string]interface{}, error) {
	raw, err := client.Call(ctx, "vm.status", vmID)
	if err != nil {
		return nil, err
	}
	var statusObj map[string]interface{}
	if err := json.Unmarshal(raw, &statusObj); err != nil {
		return nil, fmt.Errorf("failed to parse vm.status: %w", err)
	}
	return statusObj, nil
}

func optionalStringArg(args map[string]interface{}, key string) (string, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

func parseVMConsoleKeysArg(args map[string]interface{}) ([]string, error) {
	raw, exists := args["keys"]
	if !exists || raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []interface{}:
		out := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("keys[%d] must be a string", i)
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("keys must be an array of strings")
	}
}
