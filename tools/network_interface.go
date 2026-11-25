package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Interface changes are staged with interface.update and applied with
// interface.commit. With rollback on, commit arms a timer in the
// middleware (checkin_timeout seconds, 60 by default): unless
// interface.checkin is called first, the middleware restores the
// interface tables (and the IPv4 gateway) saved when the change was
// staged. The timer runs on TrueNAS, so it fires even if this server's
// connection is gone. The same methods and shapes exist on 25.04 through
// 27.
const (
	interfaceUpdateMethod   = "interface.update"
	interfaceCheckinMethod  = "interface.checkin"
	interfaceRollbackMethod = "interface.rollback"
)

const (
	defaultCheckinTimeout = 60
	minCheckinTimeout     = 15
	maxCheckinTimeout     = 600
	// maxAliasChanges caps add_aliases and remove_aliases.
	maxAliasChanges = 32
)

// How long, how often and with what per-try timeout the server tries to
// reach TrueNAS again after its connection dropped during a commit.
var (
	networkProbeBudget   = 20 * time.Second
	networkProbeInterval = time.Second
	networkProbeTimeout  = 5 * time.Second
)

type ifaceAlias struct {
	Type    string
	Address string
	Netmask int
}

func (a ifaceAlias) addr() (netip.Addr, bool) {
	addr, err := netip.ParseAddr(a.Address)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.WithZone(""), true
}

func (a ifaceAlias) prefix() (netip.Prefix, bool) {
	addr, ok := a.addr()
	if !ok {
		return netip.Prefix{}, false
	}
	p := netip.PrefixFrom(addr, a.Netmask)
	return p, p.IsValid()
}

func (a ifaceAlias) cidr() string {
	if addr, ok := a.addr(); ok {
		return fmt.Sprintf("%s/%d", addr, a.Netmask)
	}
	return fmt.Sprintf("%s/%d", a.Address, a.Netmask)
}

// parseAliases reads INET and INET6 entries of an interface.query alias
// list (configured aliases or state.aliases); LINK entries are skipped.
func parseAliases(v interface{}) []ifaceAlias {
	list, _ := v.([]interface{})
	var out []ifaceAlias
	for _, raw := range list {
		m, _ := raw.(map[string]interface{})
		typ, _ := m["type"].(string)
		addr, _ := m["address"].(string)
		if (typ != "INET" && typ != "INET6") || addr == "" {
			continue
		}
		bits, ok := aliasNetmask(m["netmask"])
		if !ok {
			continue
		}
		out = append(out, ifaceAlias{Type: typ, Address: addr, Netmask: bits})
	}
	return out
}

// aliasNetmask reads an alias prefix length (a number or a numeric string).
func aliasNetmask(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n >= 0 && n == float64(int(n)) {
			return int(n), true
		}
	case string:
		if i, err := strconv.Atoi(n); err == nil && i >= 0 {
			return i, true
		}
	}
	return 0, false
}

// unreadableAliases lists the INET and INET6 entries of an alias list whose
// prefix length cannot be read; parseAliases skips them.
func unreadableAliases(v interface{}) []string {
	list, _ := v.([]interface{})
	var out []string
	for _, raw := range list {
		m, _ := raw.(map[string]interface{})
		typ, _ := m["type"].(string)
		addr, _ := m["address"].(string)
		if (typ != "INET" && typ != "INET6") || addr == "" {
			continue
		}
		if _, ok := aliasNetmask(m["netmask"]); !ok {
			out = append(out, addr)
		}
	}
	return out
}

func aliasPayload(as []ifaceAlias) []interface{} {
	out := make([]interface{}, len(as))
	for i, a := range as {
		out[i] = map[string]interface{}{"type": a.Type, "address": a.Address, "netmask": a.Netmask}
	}
	return out
}

// parseNewAlias parses an address/prefix to add as a static alias.
func parseNewAlias(s string) (ifaceAlias, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return ifaceAlias{}, fmt.Errorf("add_aliases entry %q must be an address with a prefix length, e.g. 2001:db8::10/64 or 192.0.2.10/24", s)
	}
	addr := p.Addr()
	switch {
	case addr.Is4In6():
		return ifaceAlias{}, fmt.Errorf("add_aliases entry %q: use the plain IPv4 form", s)
	case addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast():
		return ifaceAlias{}, fmt.Errorf("add_aliases entry %q must be a unicast address", s)
	case addr.IsLinkLocalUnicast():
		return ifaceAlias{}, fmt.Errorf("add_aliases entry %q is link-local; link-local addresses are assigned automatically", s)
	case p.Bits() == 0:
		return ifaceAlias{}, fmt.Errorf("add_aliases entry %q needs a prefix length greater than 0", s)
	}
	typ := "INET"
	if addr.Is6() {
		typ = "INET6"
	}
	return ifaceAlias{Type: typ, Address: addr.String(), Netmask: p.Bits()}, nil
}

// ifaceSetting is the part of an interface's configuration this server
// changes, used to tell whether a committed change was kept or rolled back.
type ifaceSetting struct {
	Aliases  []string `json:"aliases"`
	IPv6Auto bool     `json:"ipv6_auto"`
}

func (s ifaceSetting) equal(o ifaceSetting) bool {
	if s.IPv6Auto != o.IPv6Auto || len(s.Aliases) != len(o.Aliases) {
		return false
	}
	for i := range s.Aliases {
		if s.Aliases[i] != o.Aliases[i] {
			return false
		}
	}
	return true
}

func settingOf(aliases []ifaceAlias, ipv6Auto bool) ifaceSetting {
	s := ifaceSetting{Aliases: []string{}, IPv6Auto: ipv6Auto}
	for _, a := range aliases {
		s.Aliases = append(s.Aliases, a.cidr())
	}
	sort.Strings(s.Aliases)
	return s
}

func ifaceSettingOf(iface map[string]interface{}) ifaceSetting {
	auto, _ := iface["ipv6_auto"].(bool)
	return settingOf(parseAliases(iface["aliases"]), auto)
}

// networkChange is the last interface change this server committed. It is
// held in memory only.
type networkChange struct {
	Interface   string
	Previous    ifaceSetting
	Desired     ifaceSetting
	CommittedAt time.Time
	Timeout     int
	RollbackAt  time.Time
	// Outcome is awaiting_checkin, connection_lost, checked_in,
	// rolled_back, changed_elsewhere or unknown.
	Outcome string
}

func (c *networkChange) view() map[string]interface{} {
	return map[string]interface{}{
		"interface":               c.Interface,
		"previous":                c.Previous,
		"desired":                 c.Desired,
		"committed_at":            c.CommittedAt.Format(time.RFC3339),
		"checkin_timeout_seconds": c.Timeout,
		"rollback_at":             c.RollbackAt.Format(time.RFC3339),
		"outcome":                 c.Outcome,
	}
}

func (r *Registry) setNetChange(c *networkChange) {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.netChange = c
}

// netChangeCopy returns a copy of the last committed change, or nil.
func (r *Registry) netChangeCopy() *networkChange {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.netChange == nil {
		return nil
	}
	c := *r.netChange
	return &c
}

// setNetOutcome records the outcome of the change committed at committedAt,
// unless a newer change replaced it.
func (r *Registry) setNetOutcome(committedAt time.Time, outcome string) {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.netChange != nil && r.netChange.CommittedAt.Equal(committedAt) {
		r.netChange.Outcome = outcome
	}
}

func interfaceHasPendingChanges(ctx context.Context, client *truenas.Client) (bool, error) {
	raw, err := client.Call(ctx, "interface.has_pending_changes")
	if err != nil {
		return false, err
	}
	var pending bool
	if err := json.Unmarshal(raw, &pending); err != nil {
		return false, fmt.Errorf("failed to parse interface.has_pending_changes")
	}
	return pending, nil
}

// interfaceCheckinWaiting returns the seconds left before TrueNAS rolls a
// committed change back, or nil when nothing is waiting for check-in.
func interfaceCheckinWaiting(ctx context.Context, client *truenas.Client) (*int, error) {
	raw, err := client.Call(ctx, "interface.checkin_waiting")
	if err != nil {
		return nil, err
	}
	var remaining *int
	if err := json.Unmarshal(raw, &remaining); err != nil {
		return nil, fmt.Errorf("failed to parse interface.checkin_waiting")
	}
	if remaining != nil && *remaining <= 0 {
		return nil, nil
	}
	return remaining, nil
}

func queryInterfaces(ctx context.Context, client *truenas.Client, name string) ([]map[string]interface{}, error) {
	filters := []interface{}{}
	if name != "" {
		filters = append(filters, []interface{}{"id", "=", name})
	}
	raw, err := client.Call(ctx, "interface.query", filters)
	if err != nil {
		return nil, fmt.Errorf("failed to query interfaces: %w", err)
	}
	var ifaces []map[string]interface{}
	if err := json.Unmarshal(raw, &ifaces); err != nil {
		return nil, fmt.Errorf("failed to parse interfaces")
	}
	return ifaces, nil
}

func queryInterface(ctx context.Context, client *truenas.Client, name string) (map[string]interface{}, error) {
	ifaces, err := queryInterfaces(ctx, client, name)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("interface %q not found (see query_network_interfaces)", name)
	}
	return ifaces[0], nil
}

// appliedAddresses lists the addresses on the interface right now. source
// is static (a configured alias), link_local, or dynamic (SLAAC, DHCP or
// a temporary address).
func appliedAddresses(iface map[string]interface{}) []map[string]interface{} {
	configured := map[netip.Addr]bool{}
	for _, a := range parseAliases(iface["aliases"]) {
		if addr, ok := a.addr(); ok {
			configured[addr] = true
		}
	}
	state, _ := iface["state"].(map[string]interface{})
	out := []map[string]interface{}{}
	for _, a := range parseAliases(state["aliases"]) {
		addr, ok := a.addr()
		if !ok {
			continue
		}
		family := "ipv4"
		if addr.Is6() {
			family = "ipv6"
		}
		source := "dynamic"
		switch {
		case configured[addr]:
			source = "static"
		case addr.IsLinkLocalUnicast():
			source = "link_local"
		}
		out = append(out, map[string]interface{}{"address": a.cidr(), "family": family, "source": source})
	}
	return out
}

// hasAutoconfiguredIPv6 reports whether the interface has IPv6 addresses
// that are neither configured aliases nor link-local (SLAAC or temporary).
func hasAutoconfiguredIPv6(iface map[string]interface{}) bool {
	configured := map[netip.Addr]bool{}
	for _, a := range parseAliases(iface["aliases"]) {
		if addr, ok := a.addr(); ok {
			configured[addr] = true
		}
	}
	state, _ := iface["state"].(map[string]interface{})
	for _, a := range parseAliases(state["aliases"]) {
		if addr, ok := a.addr(); ok && addr.Is6() && !addr.Is4In6() && !addr.IsLinkLocalUnicast() && !configured[addr] {
			return true
		}
	}
	return false
}

func ifaceHasAddr(iface map[string]interface{}, ip netip.Addr) bool {
	state, _ := iface["state"].(map[string]interface{})
	for _, a := range append(parseAliases(iface["aliases"]), parseAliases(state["aliases"])...) {
		if addr, ok := a.addr(); ok && addr == ip {
			return true
		}
	}
	return false
}

func interfaceView(iface map[string]interface{}, localIP string) map[string]interface{} {
	state, _ := iface["state"].(map[string]interface{})
	v := map[string]interface{}{
		"name":               iface["name"],
		"type":               iface["type"],
		"description":        iface["description"],
		"ipv4_dhcp":          iface["ipv4_dhcp"],
		"ipv6_auto":          iface["ipv6_auto"],
		"mtu":                iface["mtu"],
		"configured_aliases": ifaceSettingOf(iface).Aliases,
		"applied_addresses":  appliedAddresses(iface),
		"link_state":         state["link_state"],
		"link_address":       state["link_address"],
	}
	for _, k := range []string{"vlan_parent_interface", "vlan_tag", "lag_protocol", "lag_ports", "bridge_members"} {
		if val, ok := iface[k]; ok && val != nil {
			if l, isList := val.([]interface{}); isList && len(l) == 0 {
				continue
			}
			v[k] = val
		}
	}
	if ip, err := netip.ParseAddr(localIP); err == nil && ifaceHasAddr(iface, ip.Unmap().WithZone("")) {
		v["this_server_connection"] = true
	}
	return v
}

func (r *Registry) registerNetworkInterfaceTools() {
	r.tools["query_network_interfaces"] = Tool{
		Definition: mcp.Tool{
			Name: "query_network_interfaces",
			Description: "List network interfaces with their saved configuration and the addresses actually on them. `configured_aliases` are the static " +
				"aliases saved in TrueNAS; `applied_addresses` are read from the running system, each with `source` static, dynamic (SLAAC, DHCP or temporary) " +
				"or link_local. `ipv6_auto` is IPv6 autoconfiguration (SLAAC). `this_server_connection` marks the interface carrying this server's own " +
				"connection. `pending_changes` shows whether an interface change is staged or waiting for check-in (see get_pending_network_changes).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: one interface by name, e.g. eth0",
					},
				},
			},
		},
		Handler: handleQueryNetworkInterfaces,
	}

	r.tools["get_pending_network_changes"] = Tool{
		Definition: mcp.Tool{
			Name: "get_pending_network_changes",
			Description: "Show the state of the interface check-in window. `state` is awaiting_checkin (a committed change rolls back automatically in " +
				"`remaining_seconds` unless checkin_network_changes is called), staged (saved but not applied, for example from the web UI) or none. " +
				"`last_change` is the last change this server committed and its `outcome` (awaiting_checkin, connection_lost, checked_in, rolled_back, " +
				"changed_elsewhere or unknown), worked out by comparing the interface with the values before and after the change. Needs an API key " +
				"with the NETWORK_INTERFACE_WRITE role, because TrueNAS guards these methods with it.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: r.handleGetPendingNetworkChanges,
	}

	r.tools["update_network_interface"] = Tool{
		Definition: mcp.Tool{
			Name: "update_network_interface",
			Description: "Add or remove static IPv4/IPv6 aliases on one interface and turn IPv6 autoconfiguration (SLAAC) on or off. The change is staged " +
				"(interface.update) and applied with interface.commit with rollback on. **TrueNAS then rolls it back automatically after " +
				"checkin_timeout_seconds (default 60) unless it is checked in.** The reply has `status` awaiting_checkin, `remaining_seconds` and " +
				"`rollback_at`. Within that window: verify with query_network_interfaces (and, if you can, reach the new address from outside), then call " +
				"checkin_network_changes to keep the change, or rollback_network_changes to revert at once; doing nothing reverts it. If this server's " +
				"connection drops during the commit it reconnects and reports the state; if it cannot reconnect the reply is `connection_lost` and " +
				"TrueNAS rolls back on its own. Refused while other interface changes are pending, and when the change would remove the address this " +
				"server is connected through. Only one change can be pending at a time.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"interface": map[string]interface{}{
						"type":        "string",
						"description": "Required: interface name, e.g. eth0 (from query_network_interfaces)",
					},
					"add_aliases": map[string]interface{}{
						"type":        "array",
						"description": "Optional: static addresses to add, each address/prefix, e.g. 2001:db8::10/64 or 192.0.2.10/24",
						"items":       map[string]interface{}{"type": "string"},
						"maxItems":    maxAliasChanges,
					},
					"remove_aliases": map[string]interface{}{
						"type":        "array",
						"description": "Optional: static aliases to remove, as address/prefix or address",
						"items":       map[string]interface{}{"type": "string"},
						"maxItems":    maxAliasChanges,
					},
					"ipv6_auto": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: turn IPv6 autoconfiguration (SLAAC) on or off; omit to keep it as it is",
					},
					"checkin_timeout_seconds": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: seconds TrueNAS waits for checkin_network_changes before rolling back (default 60)",
						"minimum":     minCheckinTimeout,
						"maximum":     maxCheckinTimeout,
						"default":     defaultCheckinTimeout,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: preview without changing anything (default: false)",
						"default":     false,
					},
				},
				"required": []string{"interface"},
			},
		},
		Handler: r.handleUpdateNetworkInterfaceWithDryRun,
		Plan:    planUpdateNetworkInterface,
		Execute: r.executeUpdateNetworkInterface,
	}

	r.tools["checkin_network_changes"] = Tool{
		Definition: mcp.Tool{
			Name: "checkin_network_changes",
			Description: "Keep the interface change committed by update_network_interface: calls interface.checkin, which cancels TrueNAS's automatic " +
				"rollback. Call it only after verifying the new configuration, and before `remaining_seconds` (60 by default) runs out. Refused when " +
				"nothing is waiting for check-in, including changes that are only staged, and when the waiting change was not committed by this server " +
				"(for example one made in the web UI). A successful call also shows that this server can still reach " +
				"TrueNAS over the new configuration.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: show what is waiting for check-in without checking it in (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleCheckinNetworkChangesWithDryRun,
		Plan:    r.planCheckinNetworkChanges,
	}

	r.tools["rollback_network_changes"] = Tool{
		Definition: mcp.Tool{
			Name: "rollback_network_changes",
			Description: "Revert pending interface changes now (interface.rollback): restores the interface configuration and IPv4 gateway saved before " +
				"the changes were staged, whether they are waiting for check-in or only staged (including changes made in the web UI). Changes made " +
				"with update_network_config are not reverted. Refused when nothing is pending.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: show what would be reverted without reverting it (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleRollbackNetworkChangesWithDryRun,
		Plan:    r.planRollbackNetworkChanges,
	}
}

// pendingState reads has_pending_changes and checkin_waiting. It fails
// only when both reads fail.
func pendingState(ctx context.Context, client *truenas.Client) (map[string]interface{}, error) {
	pending, perr := interfaceHasPendingChanges(ctx, client)
	remaining, werr := interfaceCheckinWaiting(ctx, client)
	if perr != nil && werr != nil {
		return nil, fmt.Errorf("could not read pending interface changes: %w", perr)
	}
	state := "none"
	switch {
	case werr == nil && remaining != nil:
		state = "awaiting_checkin"
	case perr == nil && pending:
		state = "staged"
	}
	out := map[string]interface{}{
		"state":               state,
		"has_pending_changes": pending,
		"remaining_seconds":   remaining,
	}
	if perr != nil {
		out["has_pending_changes"] = nil
		out["has_pending_changes_error"] = perr.Error()
	}
	if werr != nil {
		out["remaining_seconds_error"] = werr.Error()
	}
	return out, nil
}

func handleQueryNetworkInterfaces(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	ifaces, err := queryInterfaces(ctx, client, name)
	if err != nil {
		return "", err
	}
	if name != "" && len(ifaces) == 0 {
		return "", fmt.Errorf("interface %q not found", name)
	}
	localIP, _ := websocketLocalIP(ctx, client)
	views := make([]map[string]interface{}, 0, len(ifaces))
	for _, iface := range ifaces {
		views = append(views, interfaceView(iface, localIP))
	}
	sort.Slice(views, func(i, j int) bool {
		a, _ := views[i]["name"].(string)
		b, _ := views[j]["name"].(string)
		return a < b
	})
	out := map[string]interface{}{"interfaces": views, "count": len(views)}
	if localIP != "" {
		out["connection_local_ip"] = localIP
	}
	if st, err := pendingState(ctx, client); err != nil {
		out["pending_changes_error"] = err.Error()
	} else {
		out["pending_changes"] = st
	}
	return marshalJSON(out)
}

// resolveOutcome compares the interface with the values before and after
// a committed change that is no longer waiting for check-in.
func resolveOutcome(ctx context.Context, client *truenas.Client, c *networkChange) string {
	iface, err := queryInterface(ctx, client, c.Interface)
	if err != nil {
		return "unknown"
	}
	switch s := ifaceSettingOf(iface); {
	case s.equal(c.Desired):
		return "checked_in"
	case s.equal(c.Previous):
		return "rolled_back"
	default:
		return "changed_elsewhere"
	}
}

func (r *Registry) handleGetPendingNetworkChanges(ctx context.Context, client *truenas.Client, _ map[string]interface{}) (string, error) {
	out, err := pendingState(ctx, client)
	if err != nil {
		return "", err
	}
	state, _ := out["state"].(string)
	switch state {
	case "awaiting_checkin":
		out["message"] = "A committed interface change is waiting for check-in. Verify it, then call checkin_network_changes before remaining_seconds runs out, or rollback_network_changes to revert now. If nothing is done TrueNAS rolls it back automatically."
	case "staged":
		out["message"] = "Interface changes are saved but not applied (not committed). This server only commits its own changes; rollback_network_changes discards these."
	default:
		out["message"] = "No interface changes are pending."
	}
	if c := r.netChangeCopy(); c != nil {
		if state == "none" && (c.Outcome == "awaiting_checkin" || c.Outcome == "connection_lost" || c.Outcome == "unknown") {
			c.Outcome = resolveOutcome(ctx, client, c)
			r.setNetOutcome(c.CommittedAt, c.Outcome)
		}
		if state == "awaiting_checkin" && (c.Outcome == "connection_lost" || c.Outcome == "unknown") {
			c.Outcome = "awaiting_checkin"
			r.setNetOutcome(c.CommittedAt, c.Outcome)
		}
		out["last_change"] = c.view()
		if iface, err := queryInterface(ctx, client, c.Interface); err == nil {
			localIP, _ := websocketLocalIP(ctx, client)
			out["interface"] = interfaceView(iface, localIP)
		}
	}
	return marshalJSON(out)
}

// trimmedStrings returns the non-blank strings of a JSON array, trimmed.
func trimmedStrings(v interface{}) []string {
	var out []string
	for _, s := range stringList(v) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func planUpdateNetworkInterface(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	name, _ := args["interface"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("interface is required")
	}
	timeout := defaultCheckinTimeout
	if v, ok := args["checkin_timeout_seconds"].(float64); ok {
		timeout = int(v)
	}
	if timeout < minCheckinTimeout || timeout > maxCheckinTimeout {
		return nil, fmt.Errorf("checkin_timeout_seconds must be between %d and %d", minCheckinTimeout, maxCheckinTimeout)
	}
	adds := trimmedStrings(args["add_aliases"])
	removes := trimmedStrings(args["remove_aliases"])
	wantAuto, hasAuto := args["ipv6_auto"].(bool)
	if len(adds) == 0 && len(removes) == 0 && !hasAuto {
		return nil, fmt.Errorf("give add_aliases, remove_aliases or ipv6_auto")
	}

	iface, err := queryInterface(ctx, client, name)
	if err != nil {
		return nil, err
	}
	id, _ := iface["id"].(string)
	if id == "" {
		id = name
	}
	current := parseAliases(iface["aliases"])
	curAuto, _ := iface["ipv6_auto"].(bool)
	ipv4DHCP, _ := iface["ipv4_dhcp"].(bool)
	state, _ := iface["state"].(map[string]interface{})
	applied := parseAliases(state["aliases"])

	next := append([]ifaceAlias{}, current...)
	var notes []string
	for _, s := range removes {
		target, bits, err := parseRemoveAlias(s)
		if err != nil {
			return nil, err
		}
		idx := -1
		for i, a := range next {
			if addr, ok := a.addr(); ok && addr == target && (bits < 0 || a.Netmask == bits) {
				idx = i
				break
			}
		}
		if idx < 0 {
			for _, a := range applied {
				if addr, ok := a.addr(); ok && addr == target {
					return nil, fmt.Errorf("%s is not a static alias on %s: it is assigned automatically (SLAAC, DHCP or temporary); turn off ipv6_auto or DHCP instead", s, name)
				}
			}
			return nil, fmt.Errorf("%s is not configured on %s", s, name)
		}
		next = append(next[:idx], next[idx+1:]...)
	}
	for _, s := range adds {
		a, err := parseNewAlias(s)
		if err != nil {
			return nil, err
		}
		addr, _ := a.addr()
		dup := false
		for _, e := range next {
			if eAddr, ok := e.addr(); ok && eAddr == addr {
				if e.Netmask != a.Netmask {
					return nil, fmt.Errorf("%s is already configured on %s as %s; remove it first", addr, name, e.cidr())
				}
				dup = true
			}
		}
		if dup {
			notes = append(notes, a.cidr()+" is already configured")
			continue
		}
		next = append(next, a)
	}
	newAuto := curAuto
	if hasAuto {
		newAuto = wantAuto
	}
	previous := settingOf(current, curAuto)
	desired := settingOf(next, newAuto)

	payload := map[string]interface{}{}
	if !settingOf(next, false).equal(settingOf(current, false)) {
		// The alias list is replaced as a whole: an alias that could not be
		// read would be dropped from it.
		if bad := unreadableAliases(iface["aliases"]); len(bad) > 0 {
			return nil, fmt.Errorf("the prefix length of configured alias %s on %s could not be read, so replacing the alias list would drop it; "+
				"nothing was changed. Fix the alias in the web UI first", strings.Join(bad, ", "), name)
		}
		payload["aliases"] = aliasPayload(next)
	}
	if newAuto != curAuto {
		payload["ipv6_auto"] = newAuto
	}
	commitOpts := map[string]interface{}{"rollback": true, "checkin_timeout": timeout}

	localIP, localErr := websocketLocalIP(ctx, client)
	var facts []string
	facts = append(facts, "interface: "+name)
	added, removed := diffStrings(previous.Aliases, desired.Aliases)
	if len(added) > 0 {
		facts = append(facts, "add: "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		facts = append(facts, "remove: "+strings.Join(removed, ", "))
	}
	if newAuto != curAuto {
		facts = append(facts, fmt.Sprintf("ipv6_auto: %t -> %t", curAuto, newAuto))
	}
	facts = append(facts, fmt.Sprintf("automatic rollback after %d seconds unless checked in", timeout))
	if localIP != "" {
		facts = append(facts, "this server is connected to "+localIP)
	}
	plan := &writePlan{
		Tool:        "update_network_interface",
		Method:      interfaceUpdateMethod,
		CallParams:  []interface{}{id, payload},
		Description: fmt.Sprintf("Stage the change to %s", name),
		Operation:   "update",
		CurrentState: map[string]interface{}{
			"interface":           name,
			"type":                iface["type"],
			"ipv4_dhcp":           ipv4DHCP,
			"ipv6_auto":           curAuto,
			"configured_aliases":  previous.Aliases,
			"applied_addresses":   appliedAddresses(iface),
			"connection_local_ip": localIP,
		},
		Warnings:   notes,
		TargetName: name,
		Bind:       map[string]interface{}{"previous": previous, "commit": commitOpts},
		Result: map[string]interface{}{
			"interface":   name,
			"previous":    previous,
			"desired":     desired,
			"commit_opts": commitOpts,
			"timeout":     timeout,
		},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Change network interface %s and apply it with a %d-second automatic rollback window", name, timeout), facts...),
	}
	if previous.equal(desired) {
		plan.Blocked = append(plan.Blocked, fmt.Sprintf("nothing to change on %s: the aliases and ipv6_auto already match", name))
		return plan, nil
	}

	pending, err := interfaceHasPendingChanges(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("could not check for pending interface changes (TrueNAS requires the NETWORK_INTERFACE_WRITE role for this): %w", err)
	}
	if pending {
		plan.Blocked = append(plan.Blocked, "interface changes are already pending (staged, or committed and waiting for check-in, possibly from the web UI); "+
			"see get_pending_network_changes, then checkin_network_changes or rollback_network_changes first")
	}

	localAddr, localParseErr := netip.ParseAddr(localIP)
	localAddr = localAddr.Unmap().WithZone("")
	if localErr != nil || localIP == "" || localParseErr != nil {
		plan.Warnings = append(plan.Warnings, "Could not tell which TrueNAS address this server is connected to; if the change removes it, the connection drops and TrueNAS rolls back after the window")
	} else if ip := localAddr; ifaceHasAddr(iface, ip) {
		if !addrRemains(ip, next, applied, current, newAuto, ipv4DHCP) {
			plan.Blocked = append(plan.Blocked, fmt.Sprintf("this server is connected to TrueNAS through %s on %s, which this change removes: the connection would drop, "+
				"this server could not check the change in, and TrueNAS would roll it back after %d seconds. Make this change in the web UI or connect through another address", ip, name, timeout))
		} else {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("This server's connection uses %s on %s. The commit reconfigures the interface and the connection may drop briefly; "+
				"if it does not come back, TrueNAS rolls back after %d seconds", ip, name, timeout))
		}
	}
	if len(next) == 0 && !newAuto && !ipv4DHCP {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s is left with no static address, no DHCP and no IPv6 autoconfiguration", name))
	}
	for _, a := range next {
		if a.Type == "INET6" && newAuto && !containsString(previous.Aliases, a.cidr()) {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("IPv6 autoconfiguration stays on: %s is added next to the SLAAC addresses", a.cidr()))
			break
		}
	}
	if curAuto || hasAutoconfiguredIPv6(iface) {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s uses IPv6 autoconfiguration (SLAAC): the commit can briefly remove the autoconfigured IPv6 addresses "+
			"until they are learned again from router advertisements, so connections over them may drop for a moment", name))
	}
	plan.Warnings = append(plan.Warnings,
		fmt.Sprintf("After the commit, TrueNAS rolls the change back automatically in %d seconds unless checkin_network_changes is called", timeout),
		"interface.commit re-applies the configuration of every interface and may restart services that listen on specific addresses")

	plan.Actions = []PlannedAction{
		{Step: 1, Description: fmt.Sprintf("Stage the new configuration of %s (not applied yet)", name), Operation: "update", Target: interfaceUpdateMethod,
			Details: map[string]interface{}{"interface": id, "aliases": desired.Aliases, "ipv6_auto": desired.IPv6Auto}},
		{Step: 2, Description: fmt.Sprintf("Apply it; TrueNAS starts a %d-second check-in window", timeout), Operation: "commit", Target: "interface.commit",
			Details: commitOpts},
		{Step: 3, Description: "Verify, then keep it with checkin_network_changes or revert with rollback_network_changes; without a check-in TrueNAS rolls back by itself",
			Operation: "checkin", Target: interfaceCheckinMethod},
	}
	plan.EstimatedTime = &EstimatedTime{MinSeconds: 1, MaxSeconds: 60,
		Note: fmt.Sprintf("The commit usually takes a few seconds; the check-in window of %d seconds starts when it returns", timeout)}
	return plan, nil
}

func parseRemoveAlias(s string) (netip.Addr, int, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Addr{}, 0, fmt.Errorf("remove_aliases entry %q must be address/prefix or address", s)
		}
		return p.Addr().WithZone(""), p.Bits(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("remove_aliases entry %q must be address/prefix or address", s)
	}
	return addr.WithZone(""), -1, nil
}

// addrRemains reports whether ip is still expected on the interface after
// the change: a static alias that stays, or an automatic address whose
// source (SLAAC or DHCP) stays on.
func addrRemains(ip netip.Addr, next, applied, current []ifaceAlias, ipv6Auto, ipv4DHCP bool) bool {
	for _, a := range next {
		if addr, ok := a.addr(); ok && addr == ip {
			return true
		}
	}
	for _, a := range current {
		if addr, ok := a.addr(); ok && addr == ip {
			return false
		}
	}
	for _, a := range applied {
		if addr, ok := a.addr(); ok && addr == ip {
			if ip.IsLinkLocalUnicast() {
				return true
			}
			if ip.Is6() {
				return ipv6Auto
			}
			return ipv4DHCP
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func diffStrings(before, after []string) (added, removed []string) {
	for _, s := range after {
		if !containsString(before, s) {
			added = append(added, s)
		}
	}
	for _, s := range before {
		if !containsString(after, s) {
			removed = append(removed, s)
		}
	}
	return added, removed
}

func (r *Registry) handleUpdateNetworkInterfaceWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateNetworkInterface(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUpdateNetworkInterface(ctx, client, args, plan)
}

// discardStaged rolls back changes left staged (saved but not committed)
// by a failed update or commit, and says what it did. A change that is
// waiting for check-in is left to TrueNAS's own timer.
func discardStaged(ctx context.Context, client *truenas.Client) string {
	pending, err := interfaceHasPendingChanges(ctx, client)
	if err != nil {
		return "could not check for staged changes (" + err.Error() + "); check get_pending_network_changes"
	}
	if !pending {
		return "no changes were left staged"
	}
	if remaining, err := interfaceCheckinWaiting(ctx, client); err == nil && remaining != nil {
		return fmt.Sprintf("a committed change is waiting for check-in and rolls back automatically in %d seconds", *remaining)
	}
	if _, err := client.Call(ctx, "interface.rollback"); err != nil {
		return "the staged change could not be discarded (" + err.Error() + "); call rollback_network_changes"
	}
	return "the staged change was discarded with interface.rollback"
}

func networkNextSteps(remaining int) []string {
	return []string{
		"Verify: query_network_interfaces (applied_addresses) and get_network_config; if you can, check that the new address is reachable from outside",
		fmt.Sprintf("Keep: call checkin_network_changes within %d seconds", remaining),
		"Revert now: call rollback_network_changes",
		"Do nothing: TrueNAS rolls the change back automatically at rollback_at",
	}
}

func (r *Registry) executeUpdateNetworkInterface(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	name, _ := plan.Result["interface"].(string)
	previous, _ := plan.Result["previous"].(ifaceSetting)
	desired, _ := plan.Result["desired"].(ifaceSetting)
	timeout, _ := plan.Result["timeout"].(int)
	commitOpts, _ := plan.Result["commit_opts"].(map[string]interface{})

	// One interface change at a time: the lock is held from the pending
	// check through the commit, so two approved calls cannot both pass it.
	if !r.netWriteMu.TryLock() {
		return "", fmt.Errorf("update_network_interface was NOT executed: another interface change from this server is in progress; nothing was changed. Call get_pending_network_changes and try again")
	}
	defer r.netWriteMu.Unlock()
	if pending, err := interfaceHasPendingChanges(ctx, client); err != nil {
		return "", fmt.Errorf("update_network_interface was NOT executed: could not check for pending interface changes: %w; nothing was changed", err)
	} else if pending {
		return "", fmt.Errorf("update_network_interface was NOT executed: interface changes are already pending; nothing was changed. See get_pending_network_changes")
	}

	gen0, _ := client.SessionGeneration(ctx)
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("interface.update failed: %w; %s; nothing was applied", err, discardStaged(ctx, client))
	}

	_, commitErr := client.Call(ctx, "interface.commit", commitOpts)
	committedAt := time.Now().UTC()
	change := &networkChange{
		Interface:   name,
		Previous:    previous,
		Desired:     desired,
		CommittedAt: committedAt,
		Timeout:     timeout,
		RollbackAt:  committedAt.Add(time.Duration(timeout) * time.Second),
		Outcome:     "awaiting_checkin",
	}

	var apiErr *truenas.APICallError
	if commitErr != nil && errors.As(commitErr, &apiErr) {
		reply := map[string]interface{}{
			"status":    "commit_failed",
			"executed":  false,
			"interface": name,
			"error":     commitErr.Error(),
			"rollback":  discardStaged(ctx, client),
		}
		if iface, err := queryInterface(ctx, client, name); err == nil {
			reply["interface_now"] = interfaceView(iface, "")
		}
		msg := fmt.Sprintf("interface.commit failed on %s: %v; TrueNAS rolls back a commit that fails to apply", name, commitErr)
		return "", replyError(msg, reply)
	}

	reconnected := false
	var remaining *int
	if commitErr != nil {
		change.Outcome = "connection_lost"
		r.setNetChange(change)
		deadline := probeDeadline(timeout)
		var reached bool
		remaining, reached = probeCheckinWaiting(ctx, client, deadline)
		if !reached {
			reply := map[string]interface{}{
				"status":      "connection_lost",
				"executed":    "unknown",
				"interface":   name,
				"error":       commitErr.Error(),
				"rollback_at": change.RollbackAt.Format(time.RFC3339),
				"message": fmt.Sprintf("The connection to TrueNAS dropped during interface.commit and could not be re-established. If the commit ran, "+
					"TrueNAS rolls the change back automatically about %d seconds after it (by %s), because this server cannot check it in. "+
					"After that time, call get_pending_network_changes to confirm the outcome.", timeout, change.RollbackAt.Format(time.RFC3339)),
			}
			return "", replyError("connection to TrueNAS lost during interface.commit; TrueNAS rolls the change back automatically", reply)
		}
		reconnected = true
		if remaining == nil {
			// TrueNAS keeps running interface.commit after the connection
			// drops and arms the check-in timer only once the apply has
			// finished. Changes still pending with no timer can mean the
			// commit is still applying, so keep looking for the timer and
			// never roll back here.
			var pending bool
			var pendErr error
			remaining, pending, pendErr = waitForCheckinTimer(ctx, client, deadline)
			if remaining == nil {
				outcome := "unknown"
				msg := "The connection dropped during interface.commit. After reconnecting, interface changes are still pending but TrueNAS is not (yet) waiting for check-in, " +
					"so the commit may still be applying. Nothing was rolled back. Call get_pending_network_changes: if it shows awaiting_checkin, verify and check in or roll back; " +
					"a committed change rolls back on its own when its window ends."
				if pendErr != nil {
					msg = "The connection dropped during interface.commit and the pending state could not be read after reconnecting (" + pendErr.Error() + "). Nothing was rolled back. Call get_pending_network_changes to see the state."
				} else if !pending {
					outcome = resolveOutcome(ctx, client, change)
					msg = "The connection dropped during interface.commit. After reconnecting nothing is pending or waiting for check-in (outcome " + outcome + ")."
				}
				r.setNetOutcome(change.CommittedAt, outcome)
				executed := interface{}("unknown")
				if outcome == "rolled_back" {
					executed = false
				}
				reply := map[string]interface{}{
					"status":    outcome,
					"executed":  executed,
					"interface": name,
					"error":     commitErr.Error(),
					"message":   msg,
				}
				if iface, err := queryInterface(ctx, client, name); err == nil {
					reply["interface_now"] = interfaceView(iface, "")
				}
				return "", replyError("the outcome of interface.commit is "+outcome+" after the connection dropped; see get_pending_network_changes", reply)
			}
		}
		change.Outcome = "awaiting_checkin"
	} else {
		var err error
		remaining, err = interfaceCheckinWaiting(ctx, client)
		if err != nil {
			remaining = nil
		}
	}
	if gen1, err := client.SessionGeneration(ctx); err == nil && gen1 != gen0 {
		reconnected = true
	}
	left := timeout
	if remaining != nil {
		left = *remaining
		change.RollbackAt = time.Now().UTC().Add(time.Duration(left) * time.Second)
	}
	r.setNetChange(change)

	reply := map[string]interface{}{
		"status":                  "awaiting_checkin",
		"executed":                true,
		"interface":               name,
		"checkin_timeout_seconds": timeout,
		"remaining_seconds":       remaining,
		"rollback_at":             change.RollbackAt.Format(time.RFC3339),
		"previous":                previous,
		"desired":                 desired,
		"reconnected":             reconnected,
		"next_steps":              networkNextSteps(left),
		"message": fmt.Sprintf("Applied to %s. TrueNAS rolls it back automatically in %d seconds unless checkin_network_changes is called.", name, left) +
			ifThen(reconnected, " The connection to TrueNAS dropped during the commit and was re-established."),
	}
	if remaining == nil {
		reply["remaining_seconds_note"] = "interface.checkin_waiting could not be read; the window is counted from the commit on this server's clock"
	}
	localIP, _ := websocketLocalIP(ctx, client)
	if iface, err := queryInterface(ctx, client, name); err == nil {
		reply["interface_now"] = interfaceView(iface, localIP)
	}
	return marshalJSON(reply)
}

func ifThen(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func replyError(msg string, reply map[string]interface{}) error {
	body, err := marshalJSON(reply)
	if err != nil {
		return errors.New(msg)
	}
	return &mcp.ReplyError{Message: msg, Reply: body}
}

// probeDeadline is when the server stops trying to reach TrueNAS after
// the connection dropped during a commit: networkProbeBudget from now,
// and never later than the check-in window.
func probeDeadline(timeout int) time.Time {
	budget := networkProbeBudget
	if w := time.Duration(timeout) * time.Second; w < budget {
		budget = w
	}
	return time.Now().Add(budget)
}

// probeCheckinWaiting tries to reach TrueNAS again after the connection
// dropped, until deadline. reached is false when every try failed.
func probeCheckinWaiting(ctx context.Context, client *truenas.Client, deadline time.Time) (remaining *int, reached bool) {
	for {
		pctx, cancel := context.WithTimeout(ctx, networkProbeTimeout)
		rem, err := interfaceCheckinWaiting(pctx, client)
		cancel()
		if err == nil {
			return rem, true
		}
		if ctx.Err() != nil || time.Now().Add(networkProbeInterval).After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(networkProbeInterval):
		}
	}
}

// waitForCheckinTimer polls after a reconnect while interface changes are
// pending but nothing waits for check-in yet, until the timer appears,
// nothing is pending any more, or deadline passes. remaining is nil when
// no timer appeared; pending and pendErr are from the last read.
func waitForCheckinTimer(ctx context.Context, client *truenas.Client, deadline time.Time) (remaining *int, pending bool, pendErr error) {
	for {
		pctx, cancel := context.WithTimeout(ctx, networkProbeTimeout)
		pending, pendErr = interfaceHasPendingChanges(pctx, client)
		if pendErr == nil && pending {
			if rem, err := interfaceCheckinWaiting(pctx, client); err == nil && rem != nil {
				cancel()
				return rem, true, nil
			}
		}
		cancel()
		if pendErr == nil && !pending {
			return nil, false, nil
		}
		if ctx.Err() != nil || time.Now().Add(networkProbeInterval).After(deadline) {
			return nil, pending, pendErr
		}
		select {
		case <-ctx.Done():
			return nil, pending, pendErr
		case <-time.After(networkProbeInterval):
		}
	}
}

// checkinGrace is how long after its expected rollback time a change this
// server committed still counts as the one waiting for check-in (the
// timer is armed when the apply finishes, after the commit was sent).
const checkinGrace = 30 * time.Second

// ownsWaitingChange reports whether the change waiting for check-in is the
// last one this server committed: it is still unresolved and its window
// has not ended.
func (r *Registry) ownsWaitingChange() bool {
	c := r.netChangeCopy()
	if c == nil {
		return false
	}
	switch c.Outcome {
	case "awaiting_checkin", "connection_lost", "unknown":
	default:
		return false
	}
	return time.Now().Before(c.RollbackAt.Add(checkinGrace))
}

func (r *Registry) planCheckinNetworkChanges(ctx context.Context, client *truenas.Client, _ map[string]interface{}) (*writePlan, error) {
	remaining, err := interfaceCheckinWaiting(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("could not read the check-in window: %w", err)
	}
	pending, _ := interfaceHasPendingChanges(ctx, client)
	current := map[string]interface{}{"remaining_seconds": remaining, "has_pending_changes": pending}
	if c := r.netChangeCopy(); c != nil {
		current["last_change"] = c.view()
	}
	plan := &writePlan{
		Tool:         "checkin_network_changes",
		Method:       interfaceCheckinMethod,
		CallParams:   []interface{}{},
		Description:  "Check in the committed interface change: TrueNAS keeps it and cancels the automatic rollback",
		Operation:    "checkin",
		CurrentState: current,
		TargetName:   "pending interface changes",
	}
	switch {
	case remaining == nil && pending:
		plan.Blocked = append(plan.Blocked, "interface changes are staged but not committed, so there is nothing to check in; checking in now would keep them saved without applying them. Use rollback_network_changes to discard them")
	case remaining == nil:
		plan.Blocked = append(plan.Blocked, "nothing is waiting for check-in: no committed change is pending (it may already have been checked in or rolled back; see get_pending_network_changes)")
	case !r.ownsWaitingChange():
		plan.Blocked = append(plan.Blocked, "the change waiting for check-in was not committed by this server (for example it was made in the web UI, or this server restarted since). "+
			"This server only checks in its own changes: check it in where it was made, or use rollback_network_changes to revert it now; otherwise TrueNAS rolls it back when the window ends")
	default:
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d seconds left before TrueNAS rolls back; after check-in there is no automatic rollback", *remaining))
	}
	return plan, nil
}

func (r *Registry) handleCheckinNetworkChangesWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := r.planCheckinNetworkChanges(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return plan.marshalDryRun()
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		var apiErr *truenas.APICallError
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("interface.checkin failed: %w", err)
		}
		return "", fmt.Errorf("could not confirm that the check-in reached TrueNAS (%w). Call get_pending_network_changes now: if remaining_seconds is still shown, call checkin_network_changes again before it runs out", err)
	}
	remaining, _ := interfaceCheckinWaiting(ctx, client)
	if remaining != nil {
		return "", fmt.Errorf("interface.checkin returned but TrueNAS still waits for check-in (%d seconds left); call checkin_network_changes again", *remaining)
	}
	reply := map[string]interface{}{
		"status":   "checked_in",
		"executed": true,
		"message":  "TrueNAS keeps the change and the automatic rollback is cancelled. This call reached TrueNAS after the change was applied, so this server's connection works.",
	}
	if c := r.netChangeCopy(); c != nil {
		outcome := resolveOutcome(ctx, client, c)
		if outcome == "rolled_back" {
			r.setNetOutcome(c.CommittedAt, outcome)
			reply["status"] = "rolled_back"
			reply["executed"] = false
			reply["message"] = "The automatic rollback ran before the check-in arrived; the interface has its previous configuration."
			return "", replyError("the change was rolled back before the check-in arrived", reply)
		}
		if outcome == "checked_in" {
			r.setNetOutcome(c.CommittedAt, outcome)
		}
		c.Outcome = outcome
		reply["last_change"] = c.view()
		if iface, err := queryInterface(ctx, client, c.Interface); err == nil {
			localIP, _ := websocketLocalIP(ctx, client)
			reply["interface_now"] = interfaceView(iface, localIP)
		}
	}
	return marshalJSON(reply)
}

func (r *Registry) planRollbackNetworkChanges(ctx context.Context, client *truenas.Client, _ map[string]interface{}) (*writePlan, error) {
	pending, err := interfaceHasPendingChanges(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("could not check for pending interface changes: %w", err)
	}
	remaining, _ := interfaceCheckinWaiting(ctx, client)
	current := map[string]interface{}{"has_pending_changes": pending, "remaining_seconds": remaining}
	if c := r.netChangeCopy(); c != nil {
		current["last_change"] = c.view()
	}
	plan := &writePlan{
		Tool:         "rollback_network_changes",
		Method:       interfaceRollbackMethod,
		CallParams:   []interface{}{},
		Description:  "Revert pending interface changes to the configuration saved before they were staged",
		Operation:    "rollback",
		CurrentState: current,
		TargetName:   "pending interface changes",
		Warnings: []string{
			"Reverts every pending interface change, including ones made outside this server (for example in the web UI), and the IPv4 gateway saved with them",
			"Changes made with update_network_config are not reverted",
		},
	}
	if !pending {
		plan.Blocked = append(plan.Blocked, "nothing to roll back: no interface changes are pending")
	}
	return plan, nil
}

func (r *Registry) handleRollbackNetworkChangesWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := r.planRollbackNetworkChanges(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return plan.marshalDryRun()
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		var apiErr *truenas.APICallError
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("interface.rollback failed: %w", err)
		}
		return "", fmt.Errorf("the connection dropped during interface.rollback (%w); call get_pending_network_changes to see whether it completed. A committed change still rolls back on its own when its window ends", err)
	}
	pending, _ := interfaceHasPendingChanges(ctx, client)
	reply := map[string]interface{}{
		"status":              "rolled_back",
		"executed":            true,
		"has_pending_changes": pending,
		"message":             "Pending interface changes were reverted.",
	}
	if c := r.netChangeCopy(); c != nil {
		if c.Outcome == "awaiting_checkin" || c.Outcome == "connection_lost" || c.Outcome == "unknown" {
			c.Outcome = resolveOutcome(ctx, client, c)
			r.setNetOutcome(c.CommittedAt, c.Outcome)
		}
		reply["last_change"] = c.view()
		if iface, err := queryInterface(ctx, client, c.Interface); err == nil {
			localIP, _ := websocketLocalIP(ctx, client)
			reply["interface_now"] = interfaceView(iface, localIP)
		}
	}
	return marshalJSON(reply)
}
