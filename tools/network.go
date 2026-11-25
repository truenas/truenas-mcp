package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// network.configuration.update writes the global network settings and
// applies them at once (route.sync, dns.sync, hostname). Unlike interface
// changes it has no check-in window and is never rolled back by TrueNAS.
const networkConfigUpdateMethod = "network.configuration.update"

// networkConfigSettable are the network.configuration fields
// update_network_config accepts. The 25.04-27 models agree on these names.
var networkConfigSettable = []string{
	"hostname", "domain", "ipv4gateway", "ipv6gateway", "nameserver1", "nameserver2", "nameserver3",
}

// networkConfigShown are the configured fields get_network_config returns.
// httpproxy (may carry credentials), hosts, activity and service
// announcement are left out.
var networkConfigShown = []string{
	"hostname", "hostname_local", "hostname_b", "hostname_virtual", "domain", "domains",
	"ipv4gateway", "ipv6gateway", "nameserver1", "nameserver2", "nameserver3",
}

var networkStateShown = []string{"ipv4gateway", "ipv6gateway", "nameserver1", "nameserver2", "nameserver3"}

func (r *Registry) registerNetworkTools() {
	r.tools["get_network_config"] = Tool{
		Definition: mcp.Tool{
			Name: "get_network_config",
			Description: "Get the global network configuration (network.configuration): hostname, domain, IPv4 and IPv6 default gateways and nameservers, " +
				"next to what is applied right now. `configured` is what is saved; `applied` is read from the running system (the first IPv4 and IPv6 default " +
				"route and the active nameservers). `gateway_status` compares them per address family: `applied` (the configured gateway is the default route), " +
				"`not_applied`, `dynamic` (no static gateway; the route comes from DHCP or IPv6 router advertisements) or `none`. `default_routes` lists every " +
				"default route with its interface. Use it to verify update_network_config.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleGetNetworkConfig,
	}

	r.tools["update_network_config"] = Tool{
		Definition: mcp.Tool{
			Name: "update_network_config",
			Description: "Change global network settings: hostname, domain, IPv4 or IPv6 default gateway, nameserver1-3. Only the fields you pass change; an empty " +
				"string clears a gateway or nameserver. **Applies immediately and is NOT rolled back automatically**: unlike update_network_interface there " +
				"is no 60-second check-in window, so a wrong gateway can cut this server's connection and stay in place. The preview shows the current " +
				"values so the change can be reverted with another call. Refused while interface changes are pending (finish them with " +
				"checkin_network_changes or rollback_network_changes first). Verify the result with get_network_config.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"hostname": map[string]interface{}{
						"type":        "string",
						"description": "Optional: new hostname (cannot be cleared; refused by TrueNAS while joined to Active Directory or IPA)",
						"minLength":   1,
					},
					"domain": map[string]interface{}{
						"type":        "string",
						"description": "Optional: DNS domain name",
					},
					"ipv4gateway": map[string]interface{}{
						"type":        "string",
						"description": "Optional: IPv4 default gateway, e.g. 192.0.2.1; empty string removes the static gateway. TrueNAS refuses a gateway no interface can reach",
					},
					"ipv6gateway": map[string]interface{}{
						"type":        "string",
						"description": "Optional: IPv6 default gateway, e.g. 2001:db8::1 or fe80::1%eth0 (a link-local gateway needs %interface); empty string removes the static gateway",
					},
					"nameserver1": map[string]interface{}{
						"type":        "string",
						"description": "Optional: primary DNS server address; empty string clears it",
					},
					"nameserver2": map[string]interface{}{
						"type":        "string",
						"description": "Optional: secondary DNS server address; empty string clears it",
					},
					"nameserver3": map[string]interface{}{
						"type":        "string",
						"description": "Optional: tertiary DNS server address; empty string clears it",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: preview without changing anything (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: handleUpdateNetworkConfigWithDryRun,
		Plan:    planUpdateNetworkConfig,
		Execute: executeUpdateNetworkConfig,
	}

	r.registerNetworkInterfaceTools()
}

func readNetworkConfig(ctx context.Context, client *truenas.Client) (map[string]interface{}, error) {
	raw, err := client.Call(ctx, "network.configuration.config")
	if err != nil {
		return nil, fmt.Errorf("failed to read network configuration: %w", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg == nil {
		return nil, fmt.Errorf("failed to parse network configuration")
	}
	return cfg, nil
}

// defaultRoutes reads the default routes from the running routing table.
func defaultRoutes(ctx context.Context, client *truenas.Client) ([]map[string]interface{}, error) {
	raw, err := client.Call(ctx, "route.system_routes", []interface{}{
		[]interface{}{"netmask", "in", []interface{}{"0.0.0.0", "::"}},
	})
	if err != nil {
		return nil, err
	}
	var routes []map[string]interface{}
	if err := json.Unmarshal(raw, &routes); err != nil {
		return nil, fmt.Errorf("failed to parse routes")
	}
	out := make([]map[string]interface{}, 0, len(routes))
	for _, rt := range routes {
		gw, _ := rt["gateway"].(string)
		if gw == "" {
			continue
		}
		family := "ipv4"
		if nm, _ := rt["netmask"].(string); nm == "::" {
			family = "ipv6"
		}
		out = append(out, map[string]interface{}{
			"family":    family,
			"gateway":   gw,
			"interface": rt["interface"],
		})
	}
	return out, nil
}

// websocketLocalIP is the address of TrueNAS this server's session is
// connected to, as the middleware sees it ("" when unknown).
func websocketLocalIP(ctx context.Context, client *truenas.Client) (string, error) {
	raw, err := client.Call(ctx, "interface.websocket_local_ip")
	if err != nil {
		return "", err
	}
	var ip string
	if err := json.Unmarshal(raw, &ip); err != nil {
		return "", nil
	}
	return ip, nil
}

func pickNetworkFields(src map[string]interface{}, keys []string) map[string]interface{} {
	out := make(map[string]interface{}, len(keys))
	for _, k := range keys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	return out
}

// sameAddr compares two address strings, ignoring case and zone.
func sameAddr(a, b string) bool {
	pa, errA := netip.ParseAddr(a)
	pb, errB := netip.ParseAddr(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(a, b)
	}
	return pa.WithZone("") == pb.WithZone("")
}

func sameAddrAndZone(a, b string) bool {
	pa, errA := netip.ParseAddr(a)
	pb, errB := netip.ParseAddr(b)
	return errA == nil && errB == nil && pa == pb
}

func gatewayStatus(configured, applied string) map[string]interface{} {
	st := map[string]interface{}{"configured": configured, "applied": applied}
	switch {
	case configured != "" && applied != "" && sameAddr(configured, applied):
		st["status"] = "applied"
	case configured != "":
		st["status"] = "not_applied"
	case applied != "":
		st["status"] = "dynamic"
	default:
		st["status"] = "none"
	}
	return st
}

// networkConfigView is the get_network_config reply, also used after an
// update so the caller sees configured and applied values side by side.
func networkConfigView(ctx context.Context, client *truenas.Client, cfg map[string]interface{}) map[string]interface{} {
	state, _ := cfg["state"].(map[string]interface{})
	str := func(m map[string]interface{}, k string) string {
		s, _ := m[k].(string)
		return s
	}
	out := map[string]interface{}{
		"configured": pickNetworkFields(cfg, networkConfigShown),
		"applied":    pickNetworkFields(state, networkStateShown),
		"gateway_status": map[string]interface{}{
			"ipv4": gatewayStatus(str(cfg, "ipv4gateway"), str(state, "ipv4gateway")),
			"ipv6": gatewayStatus(str(cfg, "ipv6gateway"), str(state, "ipv6gateway")),
		},
	}
	if routes, err := defaultRoutes(ctx, client); err != nil {
		out["default_routes_error"] = "could not read the routing table: " + err.Error()
	} else {
		out["default_routes"] = routes
	}
	if ip, err := websocketLocalIP(ctx, client); err == nil && ip != "" {
		out["connection_local_ip"] = ip
	}
	return out
}

func handleGetNetworkConfig(ctx context.Context, client *truenas.Client, _ map[string]interface{}) (string, error) {
	cfg, err := readNetworkConfig(ctx, client)
	if err != nil {
		return "", err
	}
	return marshalJSON(networkConfigView(ctx, client, cfg))
}

// normalizeNetworkSetting validates one update_network_config value and
// returns it in the form sent to TrueNAS.
func normalizeNetworkSetting(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch key {
	case "hostname":
		if value == "" {
			return "", fmt.Errorf("hostname cannot be empty")
		}
		return value, nil
	case "domain":
		return value, nil
	}
	if value == "" {
		return "", nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", fmt.Errorf("%s must be an IP address or an empty string", key)
	}
	switch {
	case key == "ipv4gateway" && !addr.Is4():
		return "", fmt.Errorf("ipv4gateway must be an IPv4 address")
	case key == "ipv6gateway" && (!addr.Is6() || addr.Is4In6()):
		return "", fmt.Errorf("ipv6gateway must be an IPv6 address")
	case key == "ipv6gateway" && addr.Zone() != "" && !addr.IsLinkLocalUnicast():
		return "", fmt.Errorf("ipv6gateway may carry a %%interface zone only for a link-local (fe80::/10) address")
	case key != "ipv6gateway" && addr.Zone() != "":
		return "", fmt.Errorf("%s must not carry a %%interface zone", key)
	case addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast():
		return "", fmt.Errorf("%s must be a unicast address that is not loopback or unspecified", key)
	}
	return addr.String(), nil
}

// interfaceIPv6Prefixes returns the IPv6 prefixes configured or applied on
// each interface, and the interfaces with IPv6 autoconfiguration on.
func interfaceIPv6Prefixes(ifaces []map[string]interface{}) (prefixes []netip.Prefix, slaac []string) {
	for _, iface := range ifaces {
		name, _ := iface["name"].(string)
		if auto, _ := iface["ipv6_auto"].(bool); auto {
			slaac = append(slaac, name)
		}
		state, _ := iface["state"].(map[string]interface{})
		for _, a := range append(parseAliases(iface["aliases"]), parseAliases(state["aliases"])...) {
			if p, ok := a.prefix(); ok && p.Addr().Is6() {
				prefixes = append(prefixes, p.Masked())
			}
		}
	}
	sort.Strings(slaac)
	return prefixes, slaac
}

func planUpdateNetworkConfig(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	want := map[string]string{}
	for _, k := range networkConfigSettable {
		v, ok := args[k].(string)
		if !ok {
			continue
		}
		norm, err := normalizeNetworkSetting(k, v)
		if err != nil {
			return nil, err
		}
		want[k] = norm
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("give at least one of %s", strings.Join(networkConfigSettable, ", "))
	}

	cfg, err := readNetworkConfig(ctx, client)
	if err != nil {
		return nil, err
	}
	changes := map[string]interface{}{}
	previous := map[string]interface{}{}
	var facts, unchanged []string
	for _, k := range networkConfigSettable {
		v, ok := want[k]
		if !ok {
			continue
		}
		cur, _ := cfg[k].(string)
		if cur == v || (k != "hostname" && k != "domain" && sameAddrAndZone(cur, v)) {
			unchanged = append(unchanged, k)
			continue
		}
		changes[k] = v
		previous[k] = cur
		facts = append(facts, fmt.Sprintf("%s: %s -> %s", k, confirmShown(k, cur), confirmShown(k, v)))
	}

	plan := &writePlan{
		Tool:        "update_network_config",
		Method:      networkConfigUpdateMethod,
		CallParams:  []interface{}{changes},
		Description: "Update the global network configuration; TrueNAS applies it immediately",
		Operation:   "update",
		CurrentState: map[string]interface{}{
			"configured": pickNetworkFields(cfg, networkConfigSettable),
			"applied":    pickNetworkFields(asStringMap(cfg["state"]), networkStateShown),
		},
		Bind:           previous,
		TargetName:     "network configuration",
		Result:         map[string]interface{}{"previous": previous},
		ConfirmSummary: newConfirmSummary("Change global network settings (applies immediately, no automatic rollback)", facts...),
	}
	if len(unchanged) > 0 {
		plan.Warnings = append(plan.Warnings, "Already set, not changed: "+strings.Join(unchanged, ", "))
	}
	if len(changes) == 0 {
		plan.Blocked = append(plan.Blocked, "nothing to change: every requested value is already set")
		return plan, nil
	}

	if pending, err := interfaceHasPendingChanges(ctx, client); err != nil {
		return nil, fmt.Errorf("could not check for pending interface changes (TrueNAS requires the NETWORK_INTERFACE_WRITE role for this): %w", err)
	} else if pending {
		plan.Blocked = append(plan.Blocked, "interface changes are pending (staged, or committed and waiting for check-in); "+
			"finish them with checkin_network_changes or rollback_network_changes first, because an interface rollback also restores the IPv4 gateway saved when they were staged")
	}

	plan.Warnings = append(plan.Warnings,
		"Applies immediately. There is no check-in window and TrueNAS does not roll this back automatically; to undo, call update_network_config again with the previous values in current_state.configured")

	localIP, _ := websocketLocalIP(ctx, client)
	var localAddr netip.Addr
	if localIP != "" {
		localAddr, _ = netip.ParseAddr(localIP)
		localAddr = localAddr.Unmap()
	}
	var ifaces []map[string]interface{}
	if raw, err := client.Call(ctx, "interface.query"); err == nil {
		_ = json.Unmarshal(raw, &ifaces)
	}
	prefixes, slaac := interfaceIPv6Prefixes(ifaces)

	for _, fam := range []struct{ key, name string }{{"ipv4gateway", "IPv4"}, {"ipv6gateway", "IPv6"}} {
		v, ok := changes[fam.key].(string)
		if !ok {
			continue
		}
		if localAddr.IsValid() && (localAddr.Is4() == (fam.key == "ipv4gateway")) {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("This server is connected to TrueNAS over %s (%s). If it reaches TrueNAS through a router, a wrong %s gateway cuts this connection and the change stays in place",
				fam.name, localIP, fam.name))
		}
		if v == "" {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("Removes the static %s default route; a route from DHCP or router advertisements may still be used", fam.name))
		}
	}
	if v, ok := changes["ipv6gateway"].(string); ok && v != "" {
		addr, _ := netip.ParseAddr(v)
		if len(slaac) > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("IPv6 autoconfiguration is on for %s; TrueNAS replaces the default route learned from router advertisements with this static gateway",
				strings.Join(slaac, ", ")))
		}
		switch {
		case addr.IsLinkLocalUnicast() && addr.Zone() == "":
			plan.Warnings = append(plan.Warnings, "A link-local IPv6 gateway without %interface may not be installable; use the form fe80::1%<interface>")
		case !addr.IsLinkLocalUnicast() && ifaces != nil && !prefixContains(prefixes, addr):
			plan.Warnings = append(plan.Warnings, "The IPv6 gateway is not inside any IPv6 prefix on this system's interfaces, so it may not be reachable; add an address in its subnet first (update_network_interface)")
		}
	}
	if _, ok := changes["hostname"]; ok {
		plan.Warnings = append(plan.Warnings, "Changing the hostname affects SMB, NFS with Kerberos and certificates that name the host; TrueNAS refuses it while joined to Active Directory or IPA")
	}

	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: "Save and apply the changed settings (routes, resolv.conf and hostname are updated at once)",
		Operation:   "update",
		Target:      networkConfigUpdateMethod,
		Details:     changes,
	}}
	return plan, nil
}

func prefixContains(prefixes []netip.Prefix, addr netip.Addr) bool {
	addr = addr.WithZone("")
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func handleUpdateNetworkConfigWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateNetworkConfig(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return plan.marshalDryRun()
	}
	return executeUpdateNetworkConfig(ctx, client, args, plan)
}

func executeUpdateNetworkConfig(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	changes, _ := plan.CallParams[0].(map[string]interface{})
	if _, err := callPlan(ctx, client, plan); err != nil {
		var apiErr *truenas.APICallError
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("TrueNAS refused the network configuration update: %w; nothing was changed", err)
		}
		return "", fmt.Errorf("network configuration update did not complete (%w); it may or may not have been applied: read get_network_config before retrying", err)
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	response := map[string]interface{}{
		"status":   "applied",
		"executed": true,
		"changed":  keys,
		"previous": plan.Result["previous"],
		"message": "Saved and applied. This change has no check-in window and is not rolled back automatically. " +
			"Compare gateway_status below; if a new gateway is not shown as applied yet, read get_network_config again in a few seconds.",
	}
	if cfg, err := readNetworkConfig(ctx, client); err != nil {
		response["verify_error"] = err.Error()
	} else {
		for k, v := range networkConfigView(ctx, client, cfg) {
			response[k] = v
		}
	}
	return marshalJSON(response)
}
