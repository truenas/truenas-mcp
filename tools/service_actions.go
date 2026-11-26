package tools

// Service write tools (control_service, set_service_enabled).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	serviceControlMethod = "service.control"
	serviceUpdateMethod  = "service.update"
)

var (
	// serviceJobWait bounds how long control_service waits for its job.
	// service.control gives up after its own 120 second default timeout.
	serviceJobWait = 150 * time.Second
	// serviceJobPoll is how often control_service reads the job.
	serviceJobPoll = time.Second
)

// serviceControlTaskTTL is how long a service.control job that outlived
// serviceJobWait is tracked as a task.
const serviceControlTaskTTL = 10 * time.Minute

var serviceControlVerbs = []string{"START", "STOP", "RESTART", "RELOAD"}

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// selfDependentServices are services this server needs in order to reach
// TrueNAS at all. Every action on them is refused, with or without
// allow_protected_service.
var selfDependentServices = map[string]string{
	"http": "it is the web server (nginx) that serves the TrueNAS API this server is connected through",
}

// protectedServices cut off access or authentication when they stop.
// STOP and RESTART (and a RELOAD that TrueNAS performs as a restart), and
// disabling them at boot, need allow_protected_service. On 25.10 only ssh
// is a service.query row; the others are listed in case a release exposes
// them.
var protectedServices = map[string]string{
	"ssh":        "SSH sessions and remote shell access to this system",
	"sssd":       "LDAP and FreeIPA logins (directory service authentication)",
	"idmap":      "Active Directory identity mapping for SMB and domain users",
	"nscd":       "user and group lookups (name service cache)",
	"keepalived": "HA failover and the virtual IP",
}

// clientServices have network clients that a stop or restart disconnects.
var clientServices = map[string]string{
	"cifs":        "Connected SMB clients are disconnected and open files may be interrupted",
	"nfs":         "NFS clients lose access to their mounts until the service is back (I/O hangs or fails)",
	"iscsitarget": "iSCSI initiators lose their LUNs until the service is back; VMs and databases on those LUNs may hang or see I/O errors",
	"nvmet":       "NVMe-oF hosts lose their namespaces until the service is back; VMs and databases on them may hang or see I/O errors",
	"ups":         "NUT clients monitoring this UPS are disconnected, and no shutdown runs on power loss while the service is stopped",
}

// reloadableServices implement a real reload in middleware (25.10
// ServiceInterface.reloadable). service.control RELOAD on any other
// service performs a full restart.
var reloadableServices = map[string]bool{
	"cifs": true, "ftp": true, "iscsitarget": true, "nfs": true, "nvmet": true, "ssh": true,
	"idmap": true, "nscd": true, "keepalived": true, "mdns": true,
}

func (r *Registry) registerServiceActionTools() {
	serviceSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc, "minLength": 1}
	}
	overrideSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["control_service"] = Tool{
		Definition: mcp.Tool{
			Name:        "control_service",
			Description: "Start, stop, restart or reload a system service (as listed by query_services, for example cifs for SMB, nfs, iscsitarget, ups, snmp, ftp, ssh) and wait for the result. Does not change whether the service starts at boot (set_service_enabled). STOP and RESTART of cifs, nfs, iscsitarget, nvmet and ups disconnect their clients; RELOAD of a service that cannot reload is a restart. STOP and RESTART of ssh (and other services that provide access or authentication) are refused unless allow_protected_service is true. Refused for services not listed by query_services and for the web server this server connects through. Also refused when it would change nothing (START of a RUNNING service, STOP or RELOAD of a STOPPED one). The preview shows the current state and boot setting.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"service": serviceSchema("Required: Service name exactly as query_services lists it (e.g. 'cifs', 'nfs', 'ssh')"),
					"action": map[string]interface{}{
						"type":        "string",
						"enum":        serviceControlVerbs,
						"description": "Required: START, STOP, RESTART or RELOAD",
					},
					"allow_protected_service": overrideSchema("Optional: Allow STOP or RESTART of a service that provides access or authentication (ssh, sssd, idmap, nscd, keepalived), which can lock users out (default: false)"),
					"dry_run":                 map[string]interface{}{"type": "boolean", "description": "Optional: Preview without changing the service (default: false)", "default": false},
				},
				"required": []string{"service", "action"},
			},
		},
		Handler: r.handleControlServiceWithDryRun,
		Plan:    planControlService,
		Execute: r.executeControlService,
	}

	r.tools["set_service_enabled"] = Tool{
		Definition: mcp.Tool{
			Name:        "set_service_enabled",
			Description: "Set whether a system service (as listed by query_services) starts at boot. Does not start or stop it now; use control_service for that. Disabling ssh (or another service that provides access or authentication) at boot is refused unless allow_protected_service is true. Refused for services not listed by query_services and for the web server this server connects through. If the service already has the requested setting, nothing is changed. The preview shows the current state and boot setting.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"service": serviceSchema("Required: Service name exactly as query_services lists it (e.g. 'cifs', 'nfs', 'ssh')"),
					"enable": map[string]interface{}{
						"type":        "boolean",
						"description": "Required: true to start the service at boot, false not to",
					},
					"allow_protected_service": overrideSchema("Optional: Allow disabling at boot a service that provides access or authentication (ssh, sssd, idmap, nscd, keepalived) (default: false)"),
					"dry_run":                 map[string]interface{}{"type": "boolean", "description": "Optional: Preview without changing the setting (default: false)", "default": false},
				},
				"required": []string{"service", "enable"},
			},
		},
		Handler: handleSetServiceEnabled,
		Plan:    planSetServiceEnabled,
	}
}

// serviceSnapshot is what the service tools read about a service and bind
// their plans to.
type serviceSnapshot struct {
	Name      string
	ID        interface{}
	State     string
	Enable    bool
	EnableSet bool
}

func (s serviceSnapshot) currentState() map[string]interface{} {
	st := map[string]interface{}{
		"service": s.Name,
		"state":   s.State,
		"running": s.State == "RUNNING",
	}
	if s.EnableSet {
		st["enabled_at_boot"] = s.Enable
	}
	return st
}

func (s serviceSnapshot) bootText() string {
	switch {
	case !s.EnableSet:
		return "unknown"
	case s.Enable:
		return "yes"
	}
	return "no"
}

// resolveService validates the name, refuses services this server depends
// on, and reads the service.query row. A name with no row is refused.
func resolveService(ctx context.Context, client *truenas.Client, args map[string]interface{}) (serviceSnapshot, error) {
	raw, _ := args["service"].(string)
	name := strings.TrimSpace(raw)
	if name == "" {
		return serviceSnapshot{}, fmt.Errorf("service is required")
	}
	if !serviceNamePattern.MatchString(name) {
		return serviceSnapshot{}, fmt.Errorf("invalid service name: use the name query_services lists (lowercase letters, digits, '_', '.', '-')")
	}
	if why, ok := selfDependentServices[name]; ok {
		return serviceSnapshot{}, fmt.Errorf("service '%s' is refused: %s, so changing it would cut this server off; nothing was changed", name, why)
	}
	row, err := lookupService(ctx, client, name)
	if err != nil {
		return serviceSnapshot{}, err
	}
	if row == nil {
		return serviceSnapshot{}, fmt.Errorf("unknown service '%s': it is not listed by query_services; nothing was changed", name)
	}
	s := serviceSnapshot{Name: name, ID: row["id"]}
	s.State, _ = row["state"].(string)
	if s.State == "" {
		s.State = "UNKNOWN"
	}
	s.Enable, s.EnableSet = row["enable"].(bool)
	return s, nil
}

func serviceBind(s serviceSnapshot, override bool) map[string]interface{} {
	return map[string]interface{}{
		"id":                      s.ID,
		"state":                   s.State,
		"enable":                  s.Enable,
		"enable_known":            s.EnableSet,
		"allow_protected_service": override,
	}
}

// ---- control_service ----

func planControlService(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	verb, _ := args["action"].(string)
	verb = strings.ToUpper(strings.TrimSpace(verb))
	valid := false
	for _, v := range serviceControlVerbs {
		valid = valid || v == verb
	}
	if !valid {
		return nil, fmt.Errorf("action must be one of %s", strings.Join(serviceControlVerbs, ", "))
	}
	override, _ := args["allow_protected_service"].(bool)
	svc, err := resolveService(ctx, client, args)
	if err != nil {
		return nil, err
	}
	name := svc.Name

	// A RELOAD that middleware performs as a stop and start is treated as
	// a RESTART by every check below.
	restartLike := verb == "STOP" || verb == "RESTART" || (verb == "RELOAD" && !reloadableServices[name])
	current := svc.currentState()
	current["action"] = verb
	current["allow_protected_service"] = override
	details := map[string]interface{}{"verb": verb, "service": name, "silent": false}

	var blocked, warnings []string
	why, protected := protectedServices[name]
	if protected {
		current["protected"] = true
	}
	switch {
	case verb == "START" && svc.State == "RUNNING":
		blocked = append(blocked, fmt.Sprintf("service '%s' is already RUNNING; START would change nothing (use RESTART or RELOAD)", name))
	case (verb == "STOP" || verb == "RELOAD") && svc.State == "STOPPED":
		blocked = append(blocked, fmt.Sprintf("service '%s' is already STOPPED; %s would change nothing (use START)", name, verb))
	}
	if protected && restartLike {
		if !override {
			blocked = append(blocked, fmt.Sprintf("service '%s' provides %s; %s could lock users out, so it needs allow_protected_service=true; nothing was changed", name, why, controlVerbText(verb, name)))
		} else {
			warnings = append(warnings, fmt.Sprintf("allow_protected_service is set: '%s' provides %s, which is interrupted (make sure you have another way in, such as the web UI or console)", name, why))
		}
	}
	if verb == "RELOAD" && !reloadableServices[name] {
		warnings = append(warnings, fmt.Sprintf("Service '%s' cannot reload: TrueNAS restarts it instead (stop, then start)", name))
		details["performed_as"] = "RESTART"
	}
	if msg, ok := clientServices[name]; ok && restartLike {
		warnings = append(warnings, msg)
	}
	if svc.State == "UNKNOWN" {
		warnings = append(warnings, fmt.Sprintf("TrueNAS could not read the state of '%s' in time (UNKNOWN); check query_services before confirming", name))
	}
	if verb == "STOP" && svc.EnableSet && svc.Enable {
		warnings = append(warnings, fmt.Sprintf("'%s' still starts at boot; use set_service_enabled to change that", name))
	}
	if verb == "START" && svc.EnableSet && !svc.Enable {
		warnings = append(warnings, fmt.Sprintf("'%s' does not start at boot, so it is stopped again after a reboot; use set_service_enabled to change that", name))
	}

	facts := []string{
		confirmFact("service", name),
		confirmFact("action", verb),
		confirmFact("state", svc.State),
		confirmFact("starts at boot", svc.bootText()),
	}
	if protected {
		facts = append(facts, confirmFact("allow_protected_service", fmt.Sprint(override)))
	}

	return &writePlan{
		Tool:         "control_service",
		Method:       serviceControlMethod,
		CallParams:   []interface{}{verb, name, map[string]interface{}{"silent": false}},
		ParamNames:   []string{"verb", "service", "options"},
		JobMethod:    serviceControlMethod,
		TargetName:   name,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("%s service '%s' (currently %s, starts at boot: %s)", verbTitle(verb), name, svc.State, svc.bootText()),
			Operation:   strings.ToLower(verb),
			Target:      name,
			Details:     details,
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		Bind:           serviceBind(svc, override),
		EstimatedTime:  &EstimatedTime{MinSeconds: 1, MaxSeconds: 120, Note: "TrueNAS gives up on a service action after 120 seconds"},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("%s service %s", verbTitle(verb), name), facts...),
		Result:         map[string]interface{}{"verb": verb},
	}, nil
}

func verbTitle(verb string) string {
	return verb[:1] + strings.ToLower(verb[1:])
}

func controlVerbText(verb, name string) string {
	return fmt.Sprintf("%s service '%s'", strings.ToLower(verb), name)
}

func (r *Registry) handleControlServiceWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planControlService(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeControlService(ctx, client, args, plan)
}

// executeControlService starts the service.control job and waits for it.
// With silent false the job fails on a failed action instead of returning
// false, but a restart or reload can still report false, which is also a
// failure.
func (r *Registry) executeControlService(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	name := plan.TargetName
	verb, _ := plan.Result["verb"].(string)
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to %s: %w", controlVerbText(verb, name), err)
	}
	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("service.control returned no job id for %s; check query_services before retrying", controlVerbText(verb, name))
	}
	response := map[string]interface{}{
		"service": name,
		"action":  verb,
		"job_id":  jobID,
	}

	job, waitErr := waitForServiceJob(ctx, client, jobID, serviceJobWait, serviceJobPoll)
	if job == nil {
		msg := fmt.Sprintf("TrueNAS accepted %s (job %d), but the job was not seen to finish", controlVerbText(verb, name), jobID)
		if waitErr != nil {
			msg += ": " + boundedErr(waitErr)
		} else {
			msg += fmt.Sprintf(" within %s", serviceJobWait)
		}
		response["status"] = "running"
		if r.taskManager != nil && ctx.Err() == nil {
			task, err := r.taskManager.CreateJobTask(ctx, "control_service", args, jobID, serviceControlTaskTTL, tasks.WithExpectedMethod(serviceControlMethod))
			if err == nil {
				response["task_id"] = task.TaskID
				response["task_status"] = task.Status
				msg += fmt.Sprintf(". Follow it with tasks_get task_id %s", task.TaskID)
			}
		}
		response["message"] = msg + ". Check query_services before retrying"
		return marshalJSON(response)
	}

	state, _ := job["state"].(string)
	if state != "SUCCESS" {
		text, _ := job["error"].(string)
		text, _ = tasks.BoundText(strings.TrimSpace(text), tasks.MaxJobTextBytes)
		if text == "" {
			text = "no error text was recorded"
		}
		return "", fmt.Errorf("failed to %s: job %d %s: %s", controlVerbText(verb, name), jobID, strings.ToLower(state), text)
	}
	ok, _ := job["result"].(bool)
	after, readErr := lookupService(ctx, client, name)
	afterState := ""
	if readErr == nil && after != nil {
		afterState, _ = after["state"].(string)
	}
	if !ok {
		msg := fmt.Sprintf("failed to %s: job %d finished but TrueNAS reports the action did not succeed", controlVerbText(verb, name), jobID)
		if afterState != "" {
			msg += fmt.Sprintf(" (state now %s)", afterState)
		}
		return "", fmt.Errorf("%s; check query_services and the TrueNAS logs", msg)
	}
	response["status"] = "completed"
	response["result"] = true
	switch {
	case readErr != nil:
		response["note"] = "The action finished, but the service could not be read back: " + boundedErr(readErr)
	case after == nil:
		response["note"] = fmt.Sprintf("The action finished, but '%s' was not found when read back", name)
	default:
		for k, v := range simplifyService(after) {
			if k != "name" {
				response[k] = v
			}
		}
	}
	msg := fmt.Sprintf("Service %s: %s finished", name, verb)
	if afterState != "" {
		msg += fmt.Sprintf("; it is now %s", afterState)
	}
	response["message"] = msg
	return marshalJSON(response)
}

// waitForServiceJob polls core.get_jobs for jobID until it ends, wait
// passes or ctx ends. It returns the finished job row, or nil with the
// reason it stopped looking. A row that is gone or runs another method, or
// a new login session (job ids restart with middleware), ends the wait:
// the id may now belong to another job.
func waitForServiceJob(ctx context.Context, client *truenas.Client, jobID int, wait, every time.Duration) (map[string]interface{}, error) {
	gen, genErr := client.SessionGeneration(ctx)
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		raw, err := client.Call(ctx, "core.get_jobs", []interface{}{[]interface{}{"id", "=", jobID}})
		var rows []map[string]interface{}
		if err == nil {
			err = json.Unmarshal(raw, &rows)
		}
		if err == nil && genErr == nil {
			if now, gerr := client.SessionGeneration(ctx); gerr == nil && now != gen {
				return nil, fmt.Errorf("the connection to TrueNAS was re-established, so job %d can no longer be identified", jobID)
			}
		}
		switch {
		case err != nil:
			lastErr = err
		case len(rows) == 0:
			return nil, fmt.Errorf("job %d is no longer listed (TrueNAS may have restarted)", jobID)
		default:
			job := rows[0]
			if m, _ := job["method"].(string); m != serviceControlMethod {
				return nil, fmt.Errorf("job %d is not a %s job (TrueNAS may have restarted)", jobID, serviceControlMethod)
			}
			switch job["state"] {
			case "SUCCESS", "FAILED", "ABORTED":
				return job, nil
			}
			lastErr = nil
		}
		if !time.Now().Before(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// ---- set_service_enabled ----

func planSetServiceEnabled(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	enable, ok := args["enable"].(bool)
	if !ok {
		return nil, fmt.Errorf("enable is required and must be true or false")
	}
	override, _ := args["allow_protected_service"].(bool)
	svc, err := resolveService(ctx, client, args)
	if err != nil {
		return nil, err
	}
	name := svc.Name
	current := svc.currentState()
	current["allow_protected_service"] = override
	why, protected := protectedServices[name]
	if protected {
		current["protected"] = true
	}
	onOff := map[bool]string{true: "start", false: "not start"}[enable]

	plan := &writePlan{
		Tool:         "set_service_enabled",
		Method:       serviceUpdateMethod,
		CallParams:   []interface{}{name, map[string]interface{}{"enable": enable}},
		ParamNames:   []string{"id_or_name", "service_update"},
		TargetName:   name,
		CurrentState: current,
		Bind:         serviceBind(svc, override),
		Result:       map[string]interface{}{"enable": enable},
	}
	if svc.EnableSet && svc.Enable == enable {
		plan.Result["unchanged"] = true
		plan.Actions = []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("No change: service '%s' is already set to %s at boot", name, onOff),
			Operation:   "update",
			Target:      name,
		}}
		plan.Warnings = []string{fmt.Sprintf("Service '%s' already has enabled_at_boot=%v; nothing will be changed", name, enable)}
		return plan, nil
	}
	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: fmt.Sprintf("Set service '%s' to %s at boot (currently %s, starts at boot: %s); it is not started or stopped now", name, onOff, svc.State, svc.bootText()),
		Operation:   "update",
		Target:      name,
		Details:     map[string]interface{}{"service": name, "enable": enable},
	}}
	if !enable && protected {
		if !override {
			plan.Blocked = []string{fmt.Sprintf("service '%s' provides %s; disabling it at boot could lock users out after a reboot, so it needs allow_protected_service=true; nothing was changed", name, why)}
			return plan, nil
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("allow_protected_service is set: after the next reboot '%s' does not start, so %s is unavailable until it is started", name, why))
	}
	if msg, ok := clientServices[name]; ok && !enable {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("After the next reboot '%s' stays stopped until it is started: %s", name, strings.ToLower(msg[:1])+msg[1:]))
	}
	if enable && svc.State != "RUNNING" {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("'%s' is %s and is not started now; use control_service START to start it", name, svc.State))
	}
	if !enable && svc.State == "RUNNING" {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("'%s' keeps running until it is stopped or the system reboots; use control_service STOP to stop it now", name))
	}
	return plan, nil
}

func handleSetServiceEnabled(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planSetServiceEnabled(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeSetServiceEnabled(ctx, client, plan)
}

func executeSetServiceEnabled(ctx context.Context, client *truenas.Client, plan *writePlan) (string, error) {
	name := plan.TargetName
	enable, _ := plan.Result["enable"].(bool)
	if unchanged, _ := plan.Result["unchanged"].(bool); unchanged {
		return marshalJSON(map[string]interface{}{
			"service":         name,
			"enabled_at_boot": enable,
			"changed":         false,
			"message":         fmt.Sprintf("Service %s already has enabled_at_boot=%v; nothing was changed", name, enable),
		})
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to set service %s enabled_at_boot=%v: %w", name, enable, err)
	}
	response := map[string]interface{}{
		"service": name,
		"changed": true,
	}
	after, err := lookupService(ctx, client, name)
	switch {
	case err != nil:
		response["enabled_at_boot"] = enable
		response["note"] = "The setting was sent, but the service could not be read back: " + boundedErr(err)
	case after == nil:
		response["enabled_at_boot"] = enable
		response["note"] = fmt.Sprintf("The setting was sent, but '%s' was not found when read back", name)
	default:
		got, ok := after["enable"].(bool)
		if !ok || got != enable {
			return "", fmt.Errorf("service.update for %s returned, but enabled_at_boot reads back as %v, not %v; check query_services", name, after["enable"], enable)
		}
		for k, v := range simplifyService(after) {
			if k != "name" {
				response[k] = v
			}
		}
	}
	verb := map[bool]string{true: "starts", false: "does not start"}[enable]
	response["message"] = fmt.Sprintf("Service %s now %s at boot; it was not started or stopped now", name, verb)
	return marshalJSON(response)
}
