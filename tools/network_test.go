package tools

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
)

// Documentation-range addresses only (RFC 3849, RFC 5737).
const (
	netTestSLAAC   = "2001:db8:1:0:a00:27ff:fe00:1"
	netTestStatic6 = "2001:db8:1::10/64"
	netTestGW6     = "2001:db8:1::1"
	netTestRAGW    = "fe80::1"
	netTestStatic4 = "192.0.2.10/24"
)

func networkConfigFixture(ipv4gw, ipv6gw string) map[string]interface{} {
	return map[string]interface{}{
		"id": 1.0, "hostname": "nas", "hostname_local": "nas", "domain": "example.org", "domains": []interface{}{},
		"ipv4gateway": ipv4gw, "ipv6gateway": ipv6gw,
		"nameserver1": "192.0.2.53", "nameserver2": "", "nameserver3": "",
		"httpproxy": "http://user:hunter2@proxy.example.org:3128", "hosts": []interface{}{},
		"state": map[string]interface{}{
			"ipv4gateway": ipv4gw, "ipv6gateway": ipv6gw,
			"nameserver1": "192.0.2.53", "nameserver2": "", "nameserver3": "", "hosts": []interface{}{},
		},
	}
}

func aliasEntry(cidr string) map[string]interface{} {
	addr, bits, _ := strings.Cut(cidr, "/")
	typ := "INET"
	if strings.Contains(addr, ":") {
		typ = "INET6"
	}
	n := 0
	for _, c := range bits {
		n = n*10 + int(c-'0')
	}
	return map[string]interface{}{"type": typ, "address": addr, "netmask": float64(n)}
}

// netIfaceFixture is an interface.query entry. static are configured
// aliases; applied adds the addresses on the interface beyond those.
func netIfaceFixture(name string, ipv6Auto bool, static, extraApplied []string) map[string]interface{} {
	configured := []interface{}{}
	stateAliases := []interface{}{map[string]interface{}{"type": "LINK", "address": "00:00:5e:00:53:01"}}
	for _, c := range static {
		configured = append(configured, aliasEntry(c))
		stateAliases = append(stateAliases, aliasEntry(c))
	}
	for _, c := range extraApplied {
		stateAliases = append(stateAliases, aliasEntry(c))
	}
	if ipv6Auto {
		stateAliases = append(stateAliases, aliasEntry(netTestSLAAC+"/64"))
	}
	stateAliases = append(stateAliases, aliasEntry("fe80::a00:27ff:fe00:1/64"))
	return map[string]interface{}{
		"id": name, "name": name, "type": "PHYSICAL", "fake": false, "description": "", "mtu": nil,
		"ipv4_dhcp": false, "ipv6_auto": ipv6Auto, "aliases": configured,
		"state": map[string]interface{}{
			"name": name, "link_state": "LINK_STATE_UP", "link_address": "00:00:5e:00:53:01", "aliases": stateAliases,
		},
	}
}

type simIface struct {
	static   []string
	ipv6Auto bool
}

// netSim models the middleware's interface staging: interface.update
// saves the original tables once and changes the saved config,
// interface.commit applies it and arms the rollback timer, checkin
// clears the snapshot, rollback (or expire) restores it.
type netSim struct {
	mu      sync.Mutex
	db      map[string]simIface
	applied map[string]simIface
	saved   map[string]simIface
	timer   int
	cfg     map[string]interface{}
	localIP string
	// commitErr fails interface.commit; commitRollsBack makes the
	// failure roll back (an apply failure) rather than leave the change
	// staged (a validation error).
	commitErr       map[string]interface{}
	commitRollsBack bool
	// armAfter, when > 0, makes interface.commit apply the change but arm
	// the check-in timer only after that many interface.checkin_waiting
	// reads, like a commit that is still applying.
	armAfter   int
	armPending int
	// pendingErr fails interface.has_pending_changes.
	pendingErr map[string]interface{}
	// stateOverride replaces values of the applied network state, like
	// a gateway TrueNAS saved but did not install.
	stateOverride map[string]interface{}
}

func newNetSim() *netSim {
	eth0 := simIface{static: []string{netTestStatic4}, ipv6Auto: true}
	return &netSim{
		db:      map[string]simIface{"eth0": eth0},
		applied: map[string]simIface{"eth0": eth0},
		cfg:     networkConfigFixture("192.0.2.1", ""),
		localIP: "192.0.2.10",
	}
}

func copyIfaces(m map[string]simIface) map[string]simIface {
	out := make(map[string]simIface, len(m))
	for k, v := range m {
		v.static = append([]string{}, v.static...)
		out[k] = v
	}
	return out
}

func (s *netSim) iface(name string) map[string]interface{} {
	db, applied := s.db[name], s.applied[name]
	var extra []string
	for _, a := range applied.static {
		if !containsString(db.static, a) {
			extra = append(extra, a)
		}
	}
	appliedStatic := []string{}
	for _, a := range db.static {
		if containsString(applied.static, a) {
			appliedStatic = append(appliedStatic, a)
		}
	}
	f := netIfaceFixture(name, applied.ipv6Auto, appliedStatic, extra)
	f["ipv6_auto"] = db.ipv6Auto
	configured := []interface{}{}
	for _, a := range db.static {
		configured = append(configured, aliasEntry(a))
	}
	f["aliases"] = configured
	return f
}

func (s *netSim) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollbackLocked()
}

func (s *netSim) rollbackLocked() {
	s.timer = 0
	if s.saved != nil {
		s.db = s.saved
		s.saved = nil
	}
	s.applied = copyIfaces(s.db)
}

func (s *netSim) dbAliases(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string{}, s.db[name].static...)
	sort.Strings(out)
	return out
}

func (s *netSim) handle(method string, raw json.RawMessage) (interface{}, map[string]interface{}, bool) {
	var params []interface{}
	_ = json.Unmarshal(raw, &params)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "interface.query":
		names := []string{}
		for n := range s.db {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(params) > 0 {
			if filters, _ := params[0].([]interface{}); len(filters) > 0 {
				f, _ := filters[0].([]interface{})
				want, _ := f[2].(string)
				names = []string{}
				if _, ok := s.db[want]; ok {
					names = []string{want}
				}
			}
		}
		out := []interface{}{}
		for _, n := range names {
			out = append(out, s.iface(n))
		}
		return out, nil, true
	case "interface.has_pending_changes":
		if s.pendingErr != nil {
			return nil, s.pendingErr, true
		}
		return s.saved != nil, nil, true
	case "interface.checkin_waiting":
		if s.armPending > 0 && s.armAfter > 0 {
			if s.armAfter--; s.armAfter == 0 {
				s.timer, s.armPending = s.armPending, 0
			}
		}
		if s.timer > 0 {
			return s.timer, nil, true
		}
		return nil, nil, true
	case "interface.websocket_local_ip":
		return s.localIP, nil, true
	case "interface.update":
		name, _ := params[0].(string)
		data, _ := params[1].(map[string]interface{})
		if s.saved == nil {
			s.saved = copyIfaces(s.db)
		}
		cur := s.db[name]
		cur.static = append([]string{}, cur.static...)
		if list, ok := data["aliases"].([]interface{}); ok {
			cur.static = nil
			for _, a := range list {
				m, _ := a.(map[string]interface{})
				cur.static = append(cur.static, ifaceAlias{Address: m["address"].(string), Netmask: int(m["netmask"].(float64))}.cidr())
			}
		}
		if v, ok := data["ipv6_auto"].(bool); ok {
			cur.ipv6Auto = v
		}
		s.db[name] = cur
		return s.iface(name), nil, true
	case "interface.commit":
		if s.commitErr != nil {
			if s.commitRollsBack {
				s.rollbackLocked()
			}
			return nil, s.commitErr, true
		}
		opts, _ := params[0].(map[string]interface{})
		s.applied = copyIfaces(s.db)
		if rb, _ := opts["rollback"].(bool); rb && s.armAfter > 0 {
			s.armPending = int(opts["checkin_timeout"].(float64))
		} else if rb {
			s.timer = int(opts["checkin_timeout"].(float64))
		} else {
			s.saved = nil
		}
		return nil, nil, true
	case "interface.checkin":
		s.timer = 0
		s.saved = nil
		return nil, nil, true
	case "interface.rollback":
		s.rollbackLocked()
		return nil, nil, true
	case "network.configuration.config":
		return s.configLocked(), nil, true
	case "network.configuration.update":
		data, _ := params[0].(map[string]interface{})
		for k, v := range data {
			s.cfg[k] = v
		}
		return s.configLocked(), nil, true
	case "route.system_routes":
		st := s.configLocked()["state"].(map[string]interface{})
		routes := []interface{}{}
		if gw, _ := st["ipv4gateway"].(string); gw != "" {
			routes = append(routes, map[string]interface{}{"network": "0.0.0.0", "netmask": "0.0.0.0", "gateway": gw, "interface": "eth0"})
		}
		if gw, _ := st["ipv6gateway"].(string); gw != "" {
			routes = append(routes, map[string]interface{}{"network": "::", "netmask": "::", "gateway": gw, "interface": "eth0"})
		}
		return routes, nil, true
	}
	return nil, nil, false
}

func (s *netSim) configLocked() map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range s.cfg {
		out[k] = v
	}
	state := map[string]interface{}{}
	for _, k := range []string{"ipv4gateway", "ipv6gateway", "nameserver1", "nameserver2", "nameserver3"} {
		state[k] = s.cfg[k]
	}
	if gw, _ := s.cfg["ipv6gateway"].(string); gw == "" && s.applied["eth0"].ipv6Auto {
		state["ipv6gateway"] = netTestRAGW
	}
	for k, v := range s.stateOverride {
		state[k] = v
	}
	out["state"] = state
	return out
}

func netRegistry(t *testing.T) (*Registry, *netSim, *fakeMiddleware) {
	t.Helper()
	sim := newNetSim()
	fake := &fakeMiddleware{handle: sim.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), sim, fake
}

func replyBody(t *testing.T, err error) map[string]interface{} {
	t.Helper()
	var re *mcp.ReplyError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a structured reply error", err)
	}
	return decode(t, re.Reply)
}

func blockedText(body map[string]interface{}) string {
	if p, ok := body["preview"].(map[string]interface{}); ok {
		body = p
	}
	return strings.Join(toStrings(body["blocked"]), "; ")
}

func callParams(t *testing.T, fake *fakeMiddleware, method string) [][]interface{} {
	t.Helper()
	var out [][]interface{}
	for _, c := range fake.called(method) {
		var p []interface{}
		if err := json.Unmarshal(c.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func withFastProbe(t *testing.T) {
	t.Helper()
	b, i, to := networkProbeBudget, networkProbeInterval, networkProbeTimeout
	networkProbeBudget, networkProbeInterval, networkProbeTimeout = 400*time.Millisecond, 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { networkProbeBudget, networkProbeInterval, networkProbeTimeout = b, i, to })
}

func TestNetworkToolClassification(t *testing.T) {
	want := map[string]Access{
		"get_network_config": Read, "query_network_interfaces": Read, "get_pending_network_changes": Read,
		"update_network_config": Destructive, "update_network_interface": Destructive,
		"checkin_network_changes": Write, "rollback_network_changes": Write,
	}
	r := NewRegistry(nil, nil)
	for name, access := range want {
		if _, ok := r.tools[name]; !ok {
			t.Errorf("%s not registered", name)
		}
		if got := safetyOf(name).Access; got != access {
			t.Errorf("%s classified %v, want %v", name, got, access)
		}
	}
	for _, name := range []string{"update_network_interface", "checkin_network_changes"} {
		if !strings.Contains(r.tools[name].Definition.Description, "60") {
			t.Errorf("%s description does not state the 60-second window", name)
		}
	}
}

func TestNetworkWritesRefusedInReadOnlyMode(t *testing.T) {
	r, sim, fake := netRegistry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for _, name := range []string{"update_network_config", "update_network_interface", "checkin_network_changes", "rollback_network_changes"} {
		if names[name] {
			t.Errorf("read-only lists %s", name)
		}
		args := toolArgsForSafetyFlagTest(name)
		args["dry_run"] = true
		if _, err := r.CallTool(t.Context(), name, args); err == nil || !strings.Contains(err.Error(), "read-only mode") {
			t.Errorf("read-only %s: err = %v", name, err)
		}
	}
	for _, name := range []string{"get_network_config", "query_network_interfaces", "get_pending_network_changes"} {
		if !names[name] {
			t.Errorf("read-only hides %s", name)
		}
		if _, err := r.CallTool(t.Context(), name, map[string]interface{}{}); err != nil {
			t.Errorf("read-only %s: %v", name, err)
		}
	}
	for _, m := range []string{"interface.update", "interface.commit", "interface.checkin", "interface.rollback", "network.configuration.update"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("read-only sent %s %d times", m, n)
		}
	}
	_ = sim
}

func TestGetNetworkConfigConfiguredVsApplied(t *testing.T) {
	r, _, _ := netRegistry(t)
	out, err := r.CallTool(t.Context(), "get_network_config", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "httpproxy") {
		t.Errorf("httpproxy leaked: %s", out)
	}
	m := decode(t, out)
	gs := m["gateway_status"].(map[string]interface{})
	if s := gs["ipv4"].(map[string]interface{})["status"]; s != "applied" {
		t.Errorf("ipv4 status = %v", s)
	}
	v6 := gs["ipv6"].(map[string]interface{})
	if v6["status"] != "dynamic" || v6["applied"] != netTestRAGW {
		t.Errorf("ipv6 status = %v", v6)
	}
	if routes := m["default_routes"].([]interface{}); len(routes) != 2 {
		t.Errorf("default_routes = %v", routes)
	}
	if m["connection_local_ip"] != "192.0.2.10" {
		t.Errorf("connection_local_ip = %v", m["connection_local_ip"])
	}
}

func TestQueryNetworkInterfacesShowsAppliedAddresses(t *testing.T) {
	r, _, _ := netRegistry(t)
	out, err := r.CallTool(t.Context(), "query_network_interfaces", map[string]interface{}{"name": "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	iface := m["interfaces"].([]interface{})[0].(map[string]interface{})
	if iface["this_server_connection"] != true || iface["ipv6_auto"] != true {
		t.Errorf("iface = %v", iface)
	}
	sources := map[string]string{}
	for _, a := range iface["applied_addresses"].([]interface{}) {
		e := a.(map[string]interface{})
		sources[e["address"].(string)] = e["source"].(string)
	}
	if sources[netTestStatic4] != "static" || sources[netTestSLAAC+"/64"] != "dynamic" || sources["fe80::a00:27ff:fe00:1/64"] != "link_local" {
		t.Errorf("sources = %v", sources)
	}
	if st := m["pending_changes"].(map[string]interface{}); st["state"] != "none" {
		t.Errorf("pending_changes = %v", st)
	}
	if _, err := r.CallTool(t.Context(), "query_network_interfaces", map[string]interface{}{"name": "eth9"}); err == nil {
		t.Error("unknown interface accepted")
	}
}

func TestUpdateNetworkConfigDryRunTokenAndVerify(t *testing.T) {
	r, _, fake := netRegistry(t)
	args := map[string]interface{}{"ipv6gateway": netTestGW6}

	out, err := r.CallTool(t.Context(), "update_network_config", map[string]interface{}{"ipv6gateway": netTestGW6, "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	preview := decode(t, out)
	if preview["dry_run"] != true || preview["executed"] != false {
		t.Errorf("preview flags: %s", out)
	}
	if !strings.Contains(out, "not roll this back automatically") || !strings.Contains(out, "IPv6 autoconfiguration is on for eth0") {
		t.Errorf("preview warnings: %s", out)
	}
	if cs := preview["confirm_summary"].(map[string]interface{}); !strings.Contains(strings.Join(toStrings(cs["facts"]), "\n"), `ipv6gateway: "" -> "2001:db8:1::1"`) {
		t.Errorf("confirm_summary = %v", cs)
	}

	out, err = r.CallTool(t.Context(), "update_network_config", args)
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); m["status"] != "confirmation_required" || m["confirmation_token"] == nil {
		t.Fatalf("no token: %s", out)
	}
	if n := len(fake.called("network.configuration.update")); n != 0 {
		t.Fatalf("update sent before confirmation (%d)", n)
	}

	// A token for other arguments is refused.
	bad := approve(t, r, "update_network_config", args)
	bad["ipv6gateway"] = "2001:db8:1::2"
	if _, err := r.CallTool(t.Context(), "update_network_config", bad); err == nil {
		t.Error("token accepted for other arguments")
	}

	out, err = r.CallTool(t.Context(), "update_network_config", approve(t, r, "update_network_config", args))
	if err != nil {
		t.Fatal(err)
	}
	calls := callParams(t, fake, "network.configuration.update")
	if len(calls) != 1 {
		t.Fatalf("update calls = %d", len(calls))
	}
	if sent := calls[0][0].(map[string]interface{}); len(sent) != 1 || sent["ipv6gateway"] != netTestGW6 {
		t.Errorf("sent %v, want only ipv6gateway", sent)
	}
	m := decode(t, out)
	if m["status"] != "applied" {
		t.Errorf("reply: %s", out)
	}
	if v6 := m["gateway_status"].(map[string]interface{})["ipv6"].(map[string]interface{}); v6["status"] != "applied" {
		t.Errorf("ipv6 after update: %v", v6)
	}
	if prev := m["previous"].(map[string]interface{}); prev["ipv6gateway"] != "" {
		t.Errorf("previous = %v", prev)
	}
}

func TestUpdateNetworkConfigRefusals(t *testing.T) {
	r, sim, fake := netRegistry(t)
	for _, args := range []map[string]interface{}{
		{"ipv6gateway": "192.0.2.1"},
		{"ipv4gateway": "2001:db8::1"},
		{"ipv6gateway": "2001:db8::1%eth0"},
		{"nameserver1": "not-an-ip"},
		{"nameserver1": "::1"},
		{"hostname": " "},
		{},
	} {
		args["dry_run"] = true
		if _, err := r.CallTool(t.Context(), "update_network_config", args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// Already set: blocked, no token.
	_, err := r.CallTool(t.Context(), "update_network_config", map[string]interface{}{"ipv4gateway": "192.0.2.1"})
	if body := replyBody(t, err); body["status"] != "blocked" || body["confirmation_token"] != nil {
		t.Errorf("no-op: %v", body)
	}
	// Pending interface changes block it.
	sim.mu.Lock()
	sim.saved = copyIfaces(sim.db)
	sim.mu.Unlock()
	_, err = r.CallTool(t.Context(), "update_network_config", map[string]interface{}{"ipv6gateway": netTestGW6})
	if body := replyBody(t, err); body["status"] != "blocked" || !strings.Contains(blockedText(body), "interface changes are pending") {
		t.Errorf("pending: %v", body)
	}
	if n := len(fake.called("network.configuration.update")); n != 0 {
		t.Errorf("update sent %d times", n)
	}
}

// The validation case: on a system with SLAAC on, add a static global /64
// alias, check it in, set the IPv6 default gateway, and verify both.
func TestNetworkInterfaceCommitCheckinThenGateway(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}

	out, err := r.CallTool(t.Context(), "update_network_interface", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	if err != nil {
		t.Fatal(err)
	}
	preview := decode(t, out)
	actions := preview["planned_actions"].([]interface{})
	if len(actions) != 3 || actions[1].(map[string]interface{})["target"] != "interface.commit" {
		t.Fatalf("actions = %v", actions)
	}
	if d := actions[1].(map[string]interface{})["details"].(map[string]interface{}); d["rollback"] != true || d["checkin_timeout"] != 60.0 {
		t.Errorf("commit details = %v", d)
	}
	if !strings.Contains(out, "rolls the change back automatically in 60 seconds") || !strings.Contains(out, "added next to the SLAAC addresses") {
		t.Errorf("preview warnings: %s", out)
	}
	if n := len(fake.called("interface.update")); n != 0 {
		t.Fatal("dry run staged a change")
	}

	out, err = r.CallTool(t.Context(), "update_network_interface", args)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, out)["status"] != "confirmation_required" || len(fake.called("interface.update")) != 0 {
		t.Fatalf("unconfirmed call: %s", out)
	}

	out, err = r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["status"] != "awaiting_checkin" || m["remaining_seconds"] != 60.0 || m["rollback_at"] == nil || m["reconnected"] != false {
		t.Fatalf("commit reply: %s", out)
	}
	upd := callParams(t, fake, "interface.update")
	if len(upd) != 1 || upd[0][0] != "eth0" {
		t.Fatalf("interface.update = %v", upd)
	}
	data := upd[0][1].(map[string]interface{})
	if _, ok := data["ipv6_auto"]; ok {
		t.Errorf("ipv6_auto sent although unchanged: %v", data)
	}
	if got := len(data["aliases"].([]interface{})); got != 2 {
		t.Errorf("aliases sent = %v", data["aliases"])
	}
	commit := callParams(t, fake, "interface.commit")
	if len(commit) != 1 || commit[0][0].(map[string]interface{})["checkin_timeout"] != 60.0 {
		t.Fatalf("interface.commit = %v", commit)
	}
	if !strings.Contains(out, `"source": "static"`) || !strings.Contains(out, netTestStatic6) {
		t.Errorf("applied addresses missing the new alias: %s", out)
	}

	out, err = r.CallTool(t.Context(), "get_pending_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); m["state"] != "awaiting_checkin" || m["remaining_seconds"] != 60.0 {
		t.Errorf("pending: %s", out)
	}
	// Interface changes are pending, so the gateway has to wait.
	if _, err := r.CallTool(t.Context(), "update_network_config", map[string]interface{}{"ipv6gateway": netTestGW6}); err == nil {
		t.Error("gateway change accepted while the interface change awaits check-in")
	}

	out, err = r.CallTool(t.Context(), "checkin_network_changes", map[string]interface{}{"dry_run": true})
	if err != nil || len(fake.called("interface.checkin")) != 0 {
		t.Fatalf("checkin dry run: %v %s", err, out)
	}
	out, err = r.CallTool(t.Context(), "checkin_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	m = decode(t, out)
	if m["status"] != "checked_in" || m["last_change"].(map[string]interface{})["outcome"] != "checked_in" {
		t.Errorf("checkin: %s", out)
	}
	sim.expire() // a late timer must not undo a checked-in change
	if got := sim.dbAliases("eth0"); len(got) != 2 {
		t.Errorf("aliases after check-in = %v", got)
	}

	out, err = r.CallTool(t.Context(), "update_network_config", approve(t, r, "update_network_config", map[string]interface{}{"ipv6gateway": netTestGW6}))
	if err != nil {
		t.Fatal(err)
	}
	if v6 := decode(t, out)["gateway_status"].(map[string]interface{})["ipv6"].(map[string]interface{}); v6["status"] != "applied" || v6["applied"] != netTestGW6 {
		t.Errorf("gateway: %v", v6)
	}
	out, err = r.CallTool(t.Context(), "query_network_interfaces", map[string]interface{}{"name": "eth0"})
	if err != nil || !strings.Contains(out, netTestStatic6) || !strings.Contains(out, netTestSLAAC) {
		t.Errorf("final interface: %v %s", err, out)
	}
}

func TestNetworkInterfaceAutomaticRollback(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}, "checkin_timeout_seconds": 30.0}
	out, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); m["remaining_seconds"] != 30.0 {
		t.Fatalf("reply: %s", out)
	}
	sim.expire()

	out, err = r.CallTool(t.Context(), "get_pending_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["state"] != "none" || m["last_change"].(map[string]interface{})["outcome"] != "rolled_back" {
		t.Errorf("after expiry: %s", out)
	}
	_, err = r.CallTool(t.Context(), "checkin_network_changes", nil)
	if body := replyBody(t, err); body["status"] != "blocked" || !strings.Contains(blockedText(body), "nothing is waiting for check-in") {
		t.Errorf("checkin after rollback: %v", body)
	}
	if n := len(fake.called("interface.checkin")); n != 0 {
		t.Errorf("checkin sent %d times", n)
	}
	if got := sim.dbAliases("eth0"); len(got) != 1 {
		t.Errorf("aliases after rollback = %v", got)
	}
}

func TestNetworkInterfaceExplicitRollback(t *testing.T) {
	r, sim, fake := netRegistry(t)
	_, err := r.CallTool(t.Context(), "rollback_network_changes", nil)
	if body := replyBody(t, err); body["status"] != "blocked" {
		t.Errorf("rollback with nothing pending: %v", body)
	}
	args := map[string]interface{}{"interface": "eth0", "ipv6_auto": false, "add_aliases": []interface{}{netTestStatic6}}
	if _, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args)); err != nil {
		t.Fatal(err)
	}
	if sent := callParams(t, fake, "interface.update")[0][1].(map[string]interface{}); sent["ipv6_auto"] != false {
		t.Errorf("ipv6_auto not sent: %v", sent)
	}
	out, err := r.CallTool(t.Context(), "rollback_network_changes", map[string]interface{}{"dry_run": true})
	if err != nil || len(fake.called("interface.rollback")) != 0 {
		t.Fatalf("rollback dry run: %v %s", err, out)
	}
	out, err = r.CallTool(t.Context(), "rollback_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["status"] != "rolled_back" || m["last_change"].(map[string]interface{})["outcome"] != "rolled_back" || m["has_pending_changes"] != false {
		t.Errorf("rollback: %s", out)
	}
	sim.mu.Lock()
	auto := sim.db["eth0"].ipv6Auto
	sim.mu.Unlock()
	if !auto || len(sim.dbAliases("eth0")) != 1 {
		t.Error("rollback did not restore eth0")
	}
}

func TestNetworkInterfaceCommitFailureDiscardsStagedChange(t *testing.T) {
	r, sim, fake := netRegistry(t)
	sim.commitErr = map[string]interface{}{"code": -32602, "message": "At least one interface must be configured"}
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	_, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	body := replyBody(t, err)
	if body["status"] != "commit_failed" || !strings.Contains(body["rollback"].(string), "discarded") {
		t.Errorf("commit failure: %v", body)
	}
	if n := len(fake.called("interface.rollback")); n != 1 {
		t.Errorf("rollback calls = %d, want 1", n)
	}
	sim.mu.Lock()
	pending := sim.saved != nil
	sim.mu.Unlock()
	if pending || len(sim.dbAliases("eth0")) != 1 {
		t.Error("staged change left behind")
	}

	// An apply failure rolls back inside commit: nothing is left to discard.
	sim.commitRollsBack = true
	_, err = r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	if body := replyBody(t, err); !strings.Contains(body["rollback"].(string), "no changes were left staged") {
		t.Errorf("apply failure: %v", body)
	}
}

func TestNetworkInterfaceConnectionDropsAndComesBack(t *testing.T) {
	withFastProbe(t)
	r, _, fake := netRegistry(t)
	var once sync.Once
	fake.drop = func(method string) bool {
		dropped := false
		if method == "interface.commit" {
			once.Do(func() { dropped = true })
		}
		return dropped
	}
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	out, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["status"] != "awaiting_checkin" || m["reconnected"] != true || m["remaining_seconds"] != 60.0 {
		t.Errorf("after reconnect: %s", out)
	}
	if out, err := r.CallTool(t.Context(), "checkin_network_changes", nil); err != nil || decode(t, out)["status"] != "checked_in" {
		t.Errorf("checkin after reconnect: %v %s", err, out)
	}
}

func TestNetworkInterfaceConnectionLostThenRolledBack(t *testing.T) {
	withFastProbe(t)
	r, sim, fake := netRegistry(t)
	fake.drop = func(method string) bool {
		if method == "interface.commit" {
			fake.refuse.Store(true)
			return true
		}
		return false
	}
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	_, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	body := replyBody(t, err)
	if body["status"] != "connection_lost" || body["rollback_at"] == nil || !strings.Contains(body["message"].(string), "rolls the change back automatically") {
		t.Fatalf("connection lost: %v", body)
	}

	// TrueNAS's own timer fires; the link comes back afterwards.
	sim.expire()
	fake.drop = nil
	fake.refuse.Store(false)
	out, err := r.CallTool(t.Context(), "get_pending_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["state"] != "none" || m["last_change"].(map[string]interface{})["outcome"] != "rolled_back" {
		t.Errorf("after reconnect: %s", out)
	}
}

func TestNetworkInterfaceRefusals(t *testing.T) {
	r, sim, fake := netRegistry(t)
	dry := func(args map[string]interface{}) (string, error) {
		return r.CallTool(t.Context(), "update_network_interface", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	}
	for _, args := range []map[string]interface{}{
		{"interface": "eth0"},
		{"interface": "eth9", "add_aliases": []interface{}{netTestStatic6}},
		{"interface": "eth0", "add_aliases": []interface{}{"2001:db8:1::10"}},
		{"interface": "eth0", "add_aliases": []interface{}{"fe80::10/64"}},
		{"interface": "eth0", "add_aliases": []interface{}{"192.0.2.10/25"}},
		{"interface": "eth0", "remove_aliases": []interface{}{"192.0.2.99"}},
		{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}, "checkin_timeout_seconds": 0.0},
	} {
		if _, err := dry(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := dry(map[string]interface{}{"interface": "eth0", "remove_aliases": []interface{}{netTestSLAAC}}); err == nil || !strings.Contains(err.Error(), "assigned automatically") {
		t.Errorf("removing a SLAAC address: %v", err)
	}
	// Removing the address this server is connected through.
	_, err := dry(map[string]interface{}{"interface": "eth0", "remove_aliases": []interface{}{netTestStatic4}})
	if body := replyBody(t, err); body["status"] != "blocked" || !strings.Contains(blockedText(body), "this server is connected to TrueNAS through 192.0.2.10") {
		t.Errorf("own address: %v", body)
	}
	// Nothing to change.
	_, err = dry(map[string]interface{}{"interface": "eth0", "ipv6_auto": true})
	if body := replyBody(t, err); body["status"] != "blocked" {
		t.Errorf("no-op: %v", body)
	}
	// Changes already pending (for example from the web UI).
	sim.mu.Lock()
	sim.saved = copyIfaces(sim.db)
	sim.mu.Unlock()
	_, err = r.CallTool(t.Context(), "update_network_interface", map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}})
	if body := replyBody(t, err); body["status"] != "blocked" || body["confirmation_token"] != nil {
		t.Errorf("pending: %v", body)
	}
	// Staged-only changes cannot be checked in.
	_, err = r.CallTool(t.Context(), "checkin_network_changes", nil)
	if body := replyBody(t, err); !strings.Contains(blockedText(body), "staged but not committed") {
		t.Errorf("checkin of staged changes: %v", body)
	}
	if n := len(fake.called("interface.update")) + len(fake.called("interface.commit")) + len(fake.called("interface.checkin")); n != 0 {
		t.Errorf("refused calls reached the middleware %d times", n)
	}
}

func TestNetworkInterfaceTokenBoundToCurrentConfig(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	call := approve(t, r, "update_network_interface", args)
	// The interface changes between preview and confirm.
	sim.mu.Lock()
	e := sim.db["eth0"]
	e.static = append(e.static, "192.0.2.20/24")
	sim.db["eth0"] = e
	sim.mu.Unlock()
	if _, err := r.CallTool(t.Context(), "update_network_interface", call); err == nil {
		t.Error("token accepted after the interface changed")
	}
	if n := len(fake.called("interface.update")); n != 0 {
		t.Errorf("interface.update sent %d times", n)
	}
}

// dropCommitOnce closes the connection once, right after interface.commit
// reached the fake (so the commit runs but its reply is lost).
func dropCommitOnce(fake *fakeMiddleware) {
	var once sync.Once
	fake.drop = func(method string) bool {
		dropped := false
		if method == "interface.commit" {
			once.Do(func() { dropped = true })
		}
		return dropped
	}
}

// After a reconnect the commit may still be applying: changes are pending
// but the check-in timer is not armed yet. The server must wait for the
// timer, not roll back.
func TestNetworkInterfaceReconnectBeforeTimerArmed(t *testing.T) {
	withFastProbe(t)
	r, sim, fake := netRegistry(t)
	sim.armAfter = 3
	dropCommitOnce(fake)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	out, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); m["status"] != "awaiting_checkin" || m["reconnected"] != true || m["remaining_seconds"] != 60.0 {
		t.Errorf("after reconnect: %s", out)
	}
	if n := len(fake.called("interface.rollback")); n != 0 {
		t.Errorf("interface.rollback sent %d times while the commit was applying", n)
	}
	if out, err := r.CallTool(t.Context(), "checkin_network_changes", nil); err != nil || decode(t, out)["status"] != "checked_in" {
		t.Errorf("checkin: %v %s", err, out)
	}
}

// If the timer never appears within the probe budget, the outcome is
// unknown and nothing is rolled back.
func TestNetworkInterfaceReconnectTimerNeverArmedIsUnknown(t *testing.T) {
	withFastProbe(t)
	r, sim, fake := netRegistry(t)
	sim.armAfter = 1 << 30
	dropCommitOnce(fake)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	_, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	body := replyBody(t, err)
	if body["status"] != "unknown" || body["executed"] != "unknown" || !strings.Contains(body["message"].(string), "get_pending_network_changes") ||
		!strings.Contains(body["message"].(string), "Nothing was rolled back") {
		t.Errorf("reply: %v", body)
	}
	if n := len(fake.called("interface.rollback")); n != 0 {
		t.Errorf("interface.rollback sent %d times", n)
	}
	sim.mu.Lock()
	staged := sim.saved != nil
	sim.mu.Unlock()
	if !staged {
		t.Error("the pending change was discarded")
	}

	// The commit finishes later and arms its timer: the change is still
	// this server's to check in.
	sim.mu.Lock()
	sim.timer, sim.armPending, sim.armAfter = sim.armPending, 0, 0
	sim.mu.Unlock()
	out, err := r.CallTool(t.Context(), "get_pending_network_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); m["state"] != "awaiting_checkin" || m["last_change"].(map[string]interface{})["outcome"] != "awaiting_checkin" {
		t.Errorf("pending after the timer armed: %s", out)
	}
	if out, err := r.CallTool(t.Context(), "checkin_network_changes", nil); err != nil || decode(t, out)["status"] != "checked_in" {
		t.Errorf("checkin: %v %s", err, out)
	}
}

func TestUpdateNetworkConfigFailsClosedWhenPendingUnreadable(t *testing.T) {
	r, sim, fake := netRegistry(t)
	sim.pendingErr = map[string]interface{}{"code": 13, "message": "Not authorized"}
	for _, args := range []map[string]interface{}{
		{"ipv6gateway": netTestGW6, "dry_run": true},
		{"ipv6gateway": netTestGW6},
	} {
		out, err := r.CallTool(t.Context(), "update_network_config", args)
		if err == nil || !strings.Contains(err.Error(), "could not check for pending interface changes") {
			t.Errorf("%v: err = %v, out = %s", args, err, out)
		}
		if strings.Contains(out, "confirmation_token") {
			t.Errorf("%v: token issued: %s", args, out)
		}
	}
	if n := len(fake.called("network.configuration.update")); n != 0 {
		t.Errorf("update sent %d times", n)
	}
}

// A change waiting for check-in that this server did not commit (for
// example from the web UI) is not checked in.
func TestCheckinRefusesChangeNotCommittedHere(t *testing.T) {
	r, sim, fake := netRegistry(t)
	webUI := func() {
		sim.mu.Lock()
		sim.saved = copyIfaces(sim.db)
		sim.timer = 60
		sim.mu.Unlock()
	}
	webUI()
	_, err := r.CallTool(t.Context(), "checkin_network_changes", nil)
	if body := replyBody(t, err); !strings.Contains(blockedText(body), "not committed by this server") {
		t.Errorf("no change of ours: %v", body)
	}
	sim.expire()

	// After this server's own change was checked in, a later change from
	// elsewhere is still refused.
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}
	if _, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool(t.Context(), "checkin_network_changes", nil); err != nil {
		t.Fatal(err)
	}
	webUI()
	_, err = r.CallTool(t.Context(), "checkin_network_changes", map[string]interface{}{"dry_run": true})
	if body := replyBody(t, err); !strings.Contains(blockedText(body), "not committed by this server") {
		t.Errorf("change from elsewhere after ours: %v", body)
	}
	if n := len(fake.called("interface.checkin")); n != 1 {
		t.Errorf("interface.checkin sent %d times, want 1 (our own change)", n)
	}
}

// An ipv6_auto-only change sends the same parameters whatever the aliases
// are, so only the plan's Bind ties the token to them.
func TestNetworkInterfaceTokenBoundToPreviousAliases(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"interface": "eth0", "ipv6_auto": false}
	call := approve(t, r, "update_network_interface", args)
	sim.mu.Lock()
	for _, m := range []map[string]simIface{sim.db, sim.applied} {
		e := m["eth0"]
		e.static = append(append([]string{}, e.static...), "192.0.2.20/24")
		m["eth0"] = e
	}
	sim.mu.Unlock()
	if _, err := r.CallTool(t.Context(), "update_network_interface", call); err == nil {
		t.Error("token accepted after the aliases changed")
	}
	if n := len(fake.called("interface.update")); n != 0 {
		t.Errorf("interface.update sent %d times", n)
	}
}

// The update sends only the new gateway, so only the plan's Bind ties the
// token to the gateway shown in the preview.
func TestUpdateNetworkConfigTokenBoundToPreviousGateway(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"ipv4gateway": "192.0.2.254"}
	call := approve(t, r, "update_network_config", args)
	sim.mu.Lock()
	sim.cfg["ipv4gateway"] = "192.0.2.2"
	sim.mu.Unlock()
	if _, err := r.CallTool(t.Context(), "update_network_config", call); err == nil {
		t.Error("token accepted after the gateway changed")
	}
	if n := len(fake.called("network.configuration.update")); n != 0 {
		t.Errorf("update sent %d times", n)
	}
}

func TestNetworkInterfaceOwnAddressChecks(t *testing.T) {
	r, sim, _ := netRegistry(t)
	dry := func(args map[string]interface{}) (string, error) {
		return r.CallTool(t.Context(), "update_network_interface", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	}
	setLocal := func(ip string) {
		sim.mu.Lock()
		sim.localIP = ip
		sim.mu.Unlock()
	}
	// Connected over a SLAAC address: turning ipv6_auto off removes it.
	setLocal(netTestSLAAC)
	_, err := dry(map[string]interface{}{"interface": "eth0", "ipv6_auto": false})
	if body := replyBody(t, err); body["status"] != "blocked" || !strings.Contains(blockedText(body), "connected to TrueNAS through "+netTestSLAAC) {
		t.Errorf("SLAAC own address: %v", body)
	}
	// An IPv4-mapped local address is the IPv4 address.
	setLocal("::ffff:192.0.2.10")
	_, err = dry(map[string]interface{}{"interface": "eth0", "remove_aliases": []interface{}{netTestStatic4}})
	if body := replyBody(t, err); body["status"] != "blocked" || !strings.Contains(blockedText(body), "connected to TrueNAS through 192.0.2.10") {
		t.Errorf("mapped own address: %v", body)
	}
	// A local address that does not parse gets the same warning as none.
	setLocal("not-an-address")
	out, err := dry(map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}})
	if err != nil || !strings.Contains(out, "Could not tell which TrueNAS address this server is connected to") {
		t.Errorf("unparsable local address: %v %s", err, out)
	}
}

func TestNetworkInterfaceUpdatesAreSerialized(t *testing.T) {
	r, sim, fake := netRegistry(t)
	args := map[string]interface{}{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}}

	// Another update holds the lock.
	r.netWriteMu.Lock()
	_, err := r.CallTool(t.Context(), "update_network_interface", approve(t, r, "update_network_interface", args))
	r.netWriteMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "another interface change") {
		t.Errorf("concurrent update: %v", err)
	}

	// A change staged after the plan was made is caught under the lock.
	plan, err := planUpdateNetworkInterface(t.Context(), r.client, args)
	if err != nil {
		t.Fatal(err)
	}
	sim.mu.Lock()
	sim.saved = copyIfaces(sim.db)
	sim.mu.Unlock()
	if _, err := r.executeUpdateNetworkInterface(t.Context(), r.client, args, plan); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Errorf("pending after plan: %v", err)
	}
	if n := len(fake.called("interface.update")); n != 0 {
		t.Errorf("interface.update sent %d times", n)
	}
}

func TestNetworkInterfaceAliasListsAreCapped(t *testing.T) {
	r, _, _ := netRegistry(t)
	many := make([]interface{}, maxAliasChanges+1)
	for i := range many {
		many[i] = "2001:db8:1::" + strconv.Itoa(i+16) + "/64"
	}
	for _, key := range []string{"add_aliases", "remove_aliases"} {
		_, err := r.CallTool(t.Context(), "update_network_interface", map[string]interface{}{"interface": "eth0", key: many, "dry_run": true})
		if err == nil || !strings.Contains(err.Error(), "at most 32 items") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}

// A configured alias whose prefix length cannot be read would be dropped
// when the alias list is replaced, so the change is refused.
func TestNetworkInterfaceRefusesUnreadableAliasNetmask(t *testing.T) {
	r, sim, fake := netRegistry(t)
	fake.handle = func(method string, raw json.RawMessage) (interface{}, map[string]interface{}, bool) {
		res, rpcErr, ok := sim.handle(method, raw)
		if method == "interface.query" {
			for _, i := range res.([]interface{}) {
				iface := i.(map[string]interface{})
				iface["aliases"] = append(iface["aliases"].([]interface{}),
					map[string]interface{}{"type": "INET6", "address": "2001:db8:1::20", "netmask": "not-a-number"})
			}
		}
		return res, rpcErr, ok
	}
	for _, args := range []map[string]interface{}{
		{"interface": "eth0", "add_aliases": []interface{}{netTestStatic6}, "dry_run": true},
		{"interface": "eth0", "remove_aliases": []interface{}{netTestStatic4}, "dry_run": true},
	} {
		_, err := r.CallTool(t.Context(), "update_network_interface", args)
		if err == nil || !strings.Contains(err.Error(), "2001:db8:1::20") || !strings.Contains(err.Error(), "could not be read") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	// An ipv6_auto-only change does not send the alias list.
	if _, err := r.CallTool(t.Context(), "update_network_interface", map[string]interface{}{"interface": "eth0", "ipv6_auto": false, "dry_run": true}); err != nil {
		t.Errorf("ipv6_auto-only change: %v", err)
	}
	if n := len(fake.called("interface.update")); n != 0 {
		t.Errorf("interface.update sent %d times", n)
	}
}

func TestUpdateNetworkConfigReportsAppliedPerGateway(t *testing.T) {
	r, sim, _ := netRegistry(t)
	// Applied.
	out, err := r.CallTool(t.Context(), "update_network_config", approve(t, r, "update_network_config", map[string]interface{}{"ipv4gateway": "192.0.2.254"}))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	gw, _ := m["changed_gateways"].(map[string]interface{})
	if v4, _ := gw["ipv4"].(map[string]interface{}); m["status"] != "applied" || v4["status"] != "applied" || gw["ipv6"] != nil {
		t.Errorf("applied: %s", out)
	}
	// Saved, but TrueNAS kept the old default route.
	sim.mu.Lock()
	sim.stateOverride = map[string]interface{}{"ipv4gateway": "192.0.2.254"}
	sim.mu.Unlock()
	out, err = r.CallTool(t.Context(), "update_network_config", approve(t, r, "update_network_config", map[string]interface{}{"ipv4gateway": "192.0.2.1"}))
	if err != nil {
		t.Fatal(err)
	}
	m = decode(t, out)
	gw, _ = m["changed_gateways"].(map[string]interface{})
	v4, _ := gw["ipv4"].(map[string]interface{})
	if m["status"] != "saved_not_applied" || v4["status"] != "not_applied" || v4["applied"] != "192.0.2.254" ||
		!strings.Contains(m["message"].(string), "not in effect") {
		t.Errorf("not applied: %s", out)
	}
}

func TestUpdateNetworkConfigGatewaySubnetWarnings(t *testing.T) {
	r, _, _ := netRegistry(t)
	dry := func(args map[string]interface{}) string {
		t.Helper()
		out, err := r.CallTool(t.Context(), "update_network_config", mergeArgs(args, map[string]interface{}{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := dry(map[string]interface{}{"ipv4gateway": "198.51.100.1"}); !strings.Contains(out, "IPv4 gateway 198.51.100.1 is not inside any IPv4 subnet") {
		t.Errorf("off-subnet IPv4 gateway: %s", out)
	}
	if out := dry(map[string]interface{}{"ipv4gateway": "192.0.2.254"}); strings.Contains(out, "not inside any") {
		t.Errorf("on-subnet IPv4 gateway warned: %s", out)
	}
	if out := dry(map[string]interface{}{"ipv6gateway": "2001:db8:99::1"}); !strings.Contains(out, "IPv6 gateway 2001:db8:99::1 is not inside any IPv6 subnet") {
		t.Errorf("off-subnet IPv6 gateway: %s", out)
	}
	// A zone-less link-local gateway gets neither warning.
	if out := dry(map[string]interface{}{"ipv6gateway": netTestRAGW}); strings.Contains(out, "installable") || strings.Contains(out, "not inside any") {
		t.Errorf("link-local gateway warned: %s", out)
	}
}
