package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func newVMGuestIPsRegistry(t *testing.T, version string, readOnly bool, handle func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool)) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"system.info": map[string]interface{}{
				"hostname": "nas", "version": version, "uptime_seconds": 100.0,
			},
		},
		handle: handle,
	}
	client, fake := newFakeClientFrom(t, fake)
	if version != "" {
		if err := client.RequireSupportedVersion(context.Background()); err != nil {
			t.Fatalf("RequireSupportedVersion(%s): %v", version, err)
		}
	}
	r := NewRegistry(client, nil)
	if err := r.SetPolicy(Policy{ReadOnly: readOnly}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	return r, fake
}

func defaultGuestIPVMQueryHandler(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	if method != "vm.query" {
		return nil, nil, false
	}
	vms := []map[string]interface{}{
		{
			"id": 1.0, "name": "stoppedvm",
			"status": map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
		},
		{
			"id": 2.0, "name": "runningvm",
			"status": map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
		},
		{
			"id": 3.0, "name": "pausedvm",
			"status": map[string]interface{}{"state": "SUSPENDED", "pid": 4322.0, "domain_state": "PAUSED"},
		},
	}
	var args []interface{}
	_ = json.Unmarshal(params, &args)
	var filters []interface{}
	if len(args) > 0 {
		filters, _ = args[0].([]interface{})
	}
	out := make([]map[string]interface{}, 0)
	for _, vm := range vms {
		if vmMatchesFilters(vm, filters) {
			out = append(out, vm)
		}
	}
	return out, nil, true
}

func TestGetVMGuestIPsVersionGating(t *testing.T) {
	for _, ver := range []string{"25.10.0", "25.10.8", "27.0.0", "27.1.0-MASTER"} {
		t.Run("gated on "+ver, func(t *testing.T) {
			r, fake := newVMGuestIPsRegistry(t, ver, false, defaultGuestIPVMQueryHandler)
			if listed(r)["get_vm_guest_ips"] {
				t.Errorf("get_vm_guest_ips should be hidden on TrueNAS %s", ver)
			}
			_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
			if err == nil {
				t.Fatalf("get_vm_guest_ips succeeded on TrueNAS %s, want version error", ver)
			}
			msg := err.Error()
			if !strings.Contains(msg, "28.0") || !strings.Contains(msg, "nothing was executed") {
				t.Errorf("get_vm_guest_ips on %s: err = %q, want friendly 28.0 requirement", ver, msg)
			}
			if strings.Contains(msg, "API error:") {
				t.Errorf("get_vm_guest_ips on %s returned raw middleware error: %q", ver, msg)
			}
			if len(fake.called("vm.query")) != 0 || len(fake.called("vm.get_guest_network_interfaces")) != 0 {
				t.Errorf("get_vm_guest_ips called middleware despite version gate")
			}
		})
	}

	t.Run("method not found fallback returns friendly error", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			rpcErr map[string]interface{}
		}{
			{"jsonrpc -32601", map[string]interface{}{"code": -32601, "message": "Method not found"}},
			{"method does not exist", map[string]interface{}{"code": -32001, "message": "Method does not exist"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// Empty version skips RequireSupportedVersion so SessionVersion is unset.
				r, _ := newVMGuestIPsRegistry(t, "", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
					if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
						return res, rpcErr, ok
					}
					if method == "vm.get_guest_network_interfaces" {
						return nil, tc.rpcErr, true
					}
					return nil, nil, false
				})
				_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
				if err == nil {
					t.Fatal("expected error for missing middleware method")
				}
				msg := err.Error()
				if !strings.Contains(msg, "TrueNAS 28.0 or later") {
					t.Errorf("err = %q, want friendly 28.0 version message", msg)
				}
				if strings.Contains(msg, "API error:") || strings.Contains(msg, "-32601") {
					t.Errorf("err leaked raw middleware error: %q", msg)
				}
			})
		}
	})
}

func TestGetVMGuestIPsReadOnlyAndAnnotations(t *testing.T) {
	r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", true, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
			return res, rpcErr, ok
		}
		if method == "vm.get_guest_network_interfaces" {
			return []map[string]interface{}{
				{
					"name":             "ens3",
					"hardware_address": "00:a0:98:11:22:33",
					"ip_addresses": []map[string]interface{}{
						{"ip_address": "192.0.2.50", "prefix": 24, "ip_address_type": "IPV4"},
					},
				},
			}, nil, true
		}
		return nil, nil, false
	})

	var found bool
	for _, def := range r.ListTools() {
		if def.Name != "get_vm_guest_ips" {
			continue
		}
		found = true
		if def.Annotations == nil {
			t.Fatal("get_vm_guest_ips missing MCP annotations")
		}
		if !def.Annotations.ReadOnlyHint {
			t.Errorf("ReadOnlyHint = false, want true")
		}
		if !def.Annotations.IdempotentHint {
			t.Errorf("IdempotentHint = false, want true")
		}
		if def.Annotations.DestructiveHint {
			t.Errorf("DestructiveHint = true, want false")
		}
		if def.Annotations.OpenWorldHint {
			t.Errorf("OpenWorldHint = true, want false")
		}
	}
	if !found {
		t.Fatal("get_vm_guest_ips should be listed in --read-only mode on TrueNAS 28.0+")
	}

	out, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
	if err != nil {
		t.Fatalf("get_vm_guest_ips failed under --read-only: %v", err)
	}
	if !strings.Contains(out, "192.0.2.50/24") {
		t.Errorf("unexpected output: %s", out)
	}
	assertNoWriteCalls(t, fake)
}

func TestGetVMGuestIPsSuccessParsing(t *testing.T) {
	r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
			return res, rpcErr, ok
		}
		if method == "vm.get_guest_network_interfaces" {
			return []map[string]interface{}{
				{
					"name":             "lo",
					"hardware_address": "00:00:00:00:00:00",
					"ip_addresses": []map[string]interface{}{
						{"ip_address": "127.0.0.1", "prefix": 8, "ip_address_type": "IPV4"},
						{"ip_address": "::1", "prefix": 128, "ip_address_type": "IPV6"},
					},
				},
				{
					"name":             "ens3",
					"hardware_address": "00:a0:98:11:22:33",
					"ip_addresses": []map[string]interface{}{
						{"ip_address": "192.0.2.50", "prefix": 24, "ip_address_type": "IPV4"},
						{"ip_address": "192.0.2.51", "prefix": 32, "ip_address_type": "ipv4"},
						{"ip_address": "2001:db8::50", "prefix": 64, "ip_address_type": "IPV6"},
						{"ip_address": "fe80::1", "prefix": 64, "ip_address_type": "ipv6"},
					},
				},
				{
					// Optional fields omitted: no hardware_address, missing prefix, missing ip_address_type.
					"name": "wg0",
					"ip_addresses": []map[string]interface{}{
						{"ip_address": "192.0.2.99"},
						{"ip_address": "2001:db8::99", "prefix": 128},
						{"ip_address": ""},
					},
				},
				{
					// Empty interface entry should be ignored.
					"name": "",
				},
			}, nil, true
		}
		return nil, nil, false
	})

	out, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
	if err != nil {
		t.Fatalf("get_vm_guest_ips failed: %v", err)
	}

	calls := fake.called("vm.get_guest_network_interfaces")
	if len(calls) != 1 {
		t.Fatalf("vm.get_guest_network_interfaces calls = %d, want 1", len(calls))
	}
	var sentParams []int
	if err := json.Unmarshal(calls[0].Params, &sentParams); err != nil || len(sentParams) != 1 || sentParams[0] != 2 {
		t.Errorf("vm.get_guest_network_interfaces params = %s, want [2]", calls[0].Params)
	}

	var resp struct {
		VMID           int                `json:"vm_id"`
		VMName         string             `json:"vm_name"`
		InterfaceCount int                `json:"interface_count"`
		Interfaces     []vmGuestInterface `json:"interfaces"`
		Note           string             `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, out)
	}
	if resp.VMID != 2 || resp.VMName != "runningvm" {
		t.Errorf("vm metadata = (%d, %q), want (2, runningvm)", resp.VMID, resp.VMName)
	}
	if resp.InterfaceCount != 3 || len(resp.Interfaces) != 3 {
		t.Fatalf("interfaces count = (%d, %d), want 3: %+v", resp.InterfaceCount, len(resp.Interfaces), resp.Interfaces)
	}
	if resp.Note != "" {
		t.Errorf("unexpected note on non-empty result: %q", resp.Note)
	}

	// Sorted alphabetically: ens3, lo, wg0.
	ens3 := resp.Interfaces[0]
	if ens3.Name != "ens3" || ens3.MAC != "00:a0:98:11:22:33" {
		t.Errorf("ens3 = %+v", ens3)
	}
	if strings.Join(ens3.IPv4, ",") != "192.0.2.50/24,192.0.2.51/32" {
		t.Errorf("ens3.IPv4 = %v", ens3.IPv4)
	}
	if strings.Join(ens3.IPv6, ",") != "2001:db8::50/64,fe80::1/64" {
		t.Errorf("ens3.IPv6 = %v", ens3.IPv6)
	}

	lo := resp.Interfaces[1]
	if lo.Name != "lo" || lo.MAC != "00:00:00:00:00:00" {
		t.Errorf("lo = %+v", lo)
	}
	if strings.Join(lo.IPv4, ",") != "127.0.0.1/8" || strings.Join(lo.IPv6, ",") != "::1/128" {
		t.Errorf("lo addrs = ipv4:%v ipv6:%v", lo.IPv4, lo.IPv6)
	}

	wg0 := resp.Interfaces[2]
	if wg0.Name != "wg0" || wg0.MAC != "" {
		t.Errorf("wg0 = %+v, want empty MAC", wg0)
	}
	if strings.Join(wg0.IPv4, ",") != "192.0.2.99" || strings.Join(wg0.IPv6, ",") != "2001:db8::99/128" {
		t.Errorf("wg0 addrs = ipv4:%v ipv6:%v", wg0.IPv4, wg0.IPv6)
	}
	// Verify omitted MAC is not serialized as empty key on wg0.
	var rawResp map[string]interface{}
	_ = json.Unmarshal([]byte(out), &rawResp)
	rawIfaces := rawResp["interfaces"].([]interface{})
	rawWG0 := rawIfaces[2].(map[string]interface{})
	if _, hasMAC := rawWG0["mac"]; hasMAC {
		t.Errorf("wg0 should omit empty mac field, got %v", rawWG0)
	}
}

func TestGetVMGuestIPsEmptyResultHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result interface{}
	}{
		{"empty slice", []interface{}{}},
		{"null result", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
					return res, rpcErr, ok
				}
				if method == "vm.get_guest_network_interfaces" {
					return tc.result, nil, true
				}
				return nil, nil, false
			})

			out, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
			if err != nil {
				t.Fatalf("get_vm_guest_ips empty result error: %v", err)
			}
			var resp map[string]interface{}
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp["interface_count"] != 0.0 {
				t.Errorf("interface_count = %v, want 0", resp["interface_count"])
			}
			ifaces, ok := resp["interfaces"].([]interface{})
			if !ok || len(ifaces) != 0 {
				t.Errorf("interfaces = %#v, want empty JSON array", resp["interfaces"])
			}
			note, _ := resp["note"].(string)
			if !strings.Contains(strings.ToLower(note), "guest agent") || !strings.Contains(strings.ToLower(note), "booting") {
				t.Errorf("note = %q, want hint about guest agent and VM booting", note)
			}
		})
	}
}

func TestGetVMGuestIPsStoppedAndSuspendedVM(t *testing.T) {
	t.Run("stopped VM rejected before calling guest interfaces", func(t *testing.T) {
		r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, defaultGuestIPVMQueryHandler)
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 1.0})
		if err == nil {
			t.Fatal("expected error for stopped VM")
		}
		if !strings.Contains(err.Error(), "STOPPED") || !strings.Contains(err.Error(), "stoppedvm") {
			t.Errorf("err = %q, want STOPPED message naming VM", err)
		}
		if len(fake.called("vm.get_guest_network_interfaces")) != 0 {
			t.Errorf("vm.get_guest_network_interfaces should not be called for a stopped VM")
		}
	})

	t.Run("suspended VM rejected before calling guest interfaces", func(t *testing.T) {
		r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, defaultGuestIPVMQueryHandler)
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 3.0})
		if err == nil {
			t.Fatal("expected error for suspended VM")
		}
		if !strings.Contains(err.Error(), "SUSPENDED") || !strings.Contains(err.Error(), "pausedvm") {
			t.Errorf("err = %q, want SUSPENDED message naming VM", err)
		}
		if len(fake.called("vm.get_guest_network_interfaces")) != 0 {
			t.Errorf("vm.get_guest_network_interfaces should not be called for a suspended VM")
		}
	})

	t.Run("VM stops between vm.query and vm.get_guest_network_interfaces", func(t *testing.T) {
		r, _ := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
				return res, rpcErr, ok
			}
			if method == "vm.get_guest_network_interfaces" {
				return nil, map[string]interface{}{"code": -32001, "message": "VM 2 is not running"}, true
			}
			return nil, nil, false
		})
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
		if err == nil {
			t.Fatal("expected error when VM is not running")
		}
		if !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "runningvm") {
			t.Errorf("err = %q, want clear not-running message", err)
		}
	})
}

func TestGetVMGuestIPsMissingVM(t *testing.T) {
	t.Run("missing in vm.query", func(t *testing.T) {
		r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, defaultGuestIPVMQueryHandler)
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 999.0})
		if err == nil {
			t.Fatal("expected error for missing VM")
		}
		if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "not found") {
			t.Errorf("err = %q, want clear not-found message", err)
		}
		if len(fake.called("vm.get_guest_network_interfaces")) != 0 {
			t.Errorf("vm.get_guest_network_interfaces should not be called for a missing VM")
		}
	})

	t.Run("deleted between vm.query and vm.get_guest_network_interfaces", func(t *testing.T) {
		r, _ := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
				return res, rpcErr, ok
			}
			if method == "vm.get_guest_network_interfaces" {
				return nil, map[string]interface{}{"code": -32001, "message": "[ENOENT] Match not found"}, true
			}
			return nil, nil, false
		})
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
		if err == nil {
			t.Fatal("expected error for deleted VM")
		}
		if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "runningvm") {
			t.Errorf("err = %q, want clear not-found message", err)
		}
	})
}

func TestGetVMGuestIPsInvalidID(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"missing id", map[string]interface{}{}},
		{"zero", map[string]interface{}{"id": 0.0}},
		{"negative", map[string]interface{}{"id": -5.0}},
		{"non-integer float", map[string]interface{}{"id": 1.5}},
		{"string", map[string]interface{}{"id": "2"}},
		{"null", map[string]interface{}{"id": nil}},
		{"boolean", map[string]interface{}{"id": true}},
	}

	for _, tc := range cases {
		t.Run("CallTool/"+tc.name, func(t *testing.T) {
			r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, defaultGuestIPVMQueryHandler)
			_, err := r.CallTool(t.Context(), "get_vm_guest_ips", tc.args)
			if err == nil {
				t.Fatalf("CallTool accepted invalid id %v", tc.args)
			}
			if len(fake.called("vm.query")) != 0 || len(fake.called("vm.get_guest_network_interfaces")) != 0 {
				t.Errorf("middleware was called for invalid id %v", tc.args)
			}
		})

		t.Run("handler/"+tc.name, func(t *testing.T) {
			client, fake := newFakeClient(t)
			_, err := handleGetVMGuestIPs(t.Context(), client, tc.args)
			if err == nil {
				t.Fatalf("handleGetVMGuestIPs accepted invalid id %v", tc.args)
			}
			if !strings.Contains(err.Error(), "positive integer") {
				t.Errorf("err = %q, want positive integer validation error", err)
			}
			if len(fake.called("vm.query")) != 0 || len(fake.called("vm.get_guest_network_interfaces")) != 0 {
				t.Errorf("middleware was called for invalid id %v", tc.args)
			}
		})
	}
}

func TestGetVMGuestIPsMiddlewareErrors(t *testing.T) {
	t.Run("vm.query failure", func(t *testing.T) {
		r, fake := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "vm.query" {
				return nil, map[string]interface{}{"code": -32001, "message": "database unavailable"}, true
			}
			return nil, nil, false
		})
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "database unavailable") {
			t.Errorf("err = %v, want vm.query error", err)
		}
		if len(fake.called("vm.get_guest_network_interfaces")) != 0 {
			t.Errorf("vm.get_guest_network_interfaces should not be called after vm.query failure")
		}
	})

	t.Run("guest agent call error", func(t *testing.T) {
		r, _ := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
				return res, rpcErr, ok
			}
			if method == "vm.get_guest_network_interfaces" {
				return nil, map[string]interface{}{"code": -32001, "message": "VM 2: QEMU guest agent is not connected"}, true
			}
			return nil, nil, false
		})
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "QEMU guest agent is not connected") {
			t.Errorf("err = %v, want guest agent error", err)
		}
	})

	t.Run("unexpected response shape", func(t *testing.T) {
		r, _ := newVMGuestIPsRegistry(t, "28.0.0-MASTER", false, func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if res, rpcErr, ok := defaultGuestIPVMQueryHandler(method, params); ok {
				return res, rpcErr, ok
			}
			if method == "vm.get_guest_network_interfaces" {
				return map[string]interface{}{"unexpected": true}, nil, true
			}
			return nil, nil, false
		})
		_, err := r.CallTool(t.Context(), "get_vm_guest_ips", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "failed to parse guest network interfaces") {
			t.Errorf("err = %v, want parse error", err)
		}
	})
}
