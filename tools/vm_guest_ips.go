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

func (r *Registry) registerVMGuestIPTools() {
	r.tools["get_vm_guest_ips"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_vm_guest_ips",
			Description: "Read a running virtual machine's guest network interfaces and IPv4/IPv6 addresses via the QEMU guest agent. Requires TrueNAS 28.0 or later and qemu-guest-agent running inside the guest. Read-only.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{
						"type":        "integer",
						"description": "Numeric VM ID (from query_vms)",
						"minimum":     1,
					},
				},
				"required": []string{"id"},
			},
		},
		Handler: handleGetVMGuestIPs,
	}
}

// rawVMGuestIPAddress mirrors VMGuestNetworkInterfaceIPAddress in TrueNAS
// middleware (api/v28_0_0/vm.py). Pointer Prefix tolerates an omitted field.
type rawVMGuestIPAddress struct {
	IPAddress     string `json:"ip_address"`
	Prefix        *int   `json:"prefix"`
	IPAddressType string `json:"ip_address_type"`
}

// rawVMGuestInterface mirrors VMGuestNetworkInterface in TrueNAS middleware
// (api/v28_0_0/vm.py).
type rawVMGuestInterface struct {
	Name            string                `json:"name"`
	HardwareAddress string                `json:"hardware_address"`
	IPAddresses     []rawVMGuestIPAddress `json:"ip_addresses"`
}

type vmGuestInterface struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	IPv4  []string `json:"ipv4,omitempty"`
	IPv6  []string `json:"ipv6,omitempty"`
	Other []string `json:"other,omitempty"`
}

func handleGetVMGuestIPs(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	vmID, hasID, err := vmIntArg(args, "id", 0)
	if err != nil || !hasID || vmID < 1 {
		return "", fmt.Errorf("id must be a positive integer (>= 1)")
	}

	rawVM, err := client.Call(ctx, "vm.query",
		[]interface{}{[]interface{}{"id", "=", vmID}},
		map[string]interface{}{"select": []interface{}{"id", "name", "status"}},
	)
	if err != nil {
		return "", fmt.Errorf("failed to query VM %d: %w", vmID, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rawVM, &rows); err != nil {
		return "", fmt.Errorf("failed to parse vm.query response: %w", err)
	}
	matched, err := matchVMRow(rows, vmID, true, "", false)
	if err != nil {
		return "", err
	}

	vmName, _ := matched["name"].(string)
	if status, ok := matched["status"].(map[string]interface{}); ok {
		if state, ok := status["state"].(string); ok {
			state = strings.ToUpper(strings.TrimSpace(state))
			if state == "STOPPED" {
				return "", fmt.Errorf("VM %s is STOPPED; start the VM to read guest network interfaces", formatVMLabel(vmName, vmID))
			}
			if state != "" && state != "RUNNING" {
				return "", fmt.Errorf("VM %s is %s (not RUNNING); start or resume the VM to read guest network interfaces", formatVMLabel(vmName, vmID), state)
			}
		}
	}

	rawIfaces, err := client.Call(ctx, "vm.get_guest_network_interfaces", vmID)
	if err != nil {
		if isMethodNotFoundErr(err) {
			return "", needsNewerTrueNASError("get_vm_guest_ips", "vm.get_guest_network_interfaces")
		}
		if isVMMissingErr(err) {
			return "", fmt.Errorf("VM %s not found", formatVMLabel(vmName, vmID))
		}
		if isVMNotRunningErr(err) {
			return "", fmt.Errorf("VM %s is not running; start the VM to read guest network interfaces", formatVMLabel(vmName, vmID))
		}
		if isGuestAgentErr(err) {
			return "", fmt.Errorf("failed to get guest network interfaces for VM %s: %w; the QEMU guest agent did not answer: install and start qemu-guest-agent in the guest, or retry if the guest is still booting", formatVMLabel(vmName, vmID), err)
		}
		return "", fmt.Errorf("failed to get guest network interfaces for VM %s: %w", formatVMLabel(vmName, vmID), err)
	}

	var parsed []rawVMGuestInterface
	if err := json.Unmarshal(rawIfaces, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse guest network interfaces for VM %s: %w", formatVMLabel(vmName, vmID), err)
	}

	interfaces := formatVMGuestInterfaces(parsed)
	response := map[string]interface{}{
		"vm_id":           vmID,
		"interface_count": len(interfaces),
		"interfaces":      interfaces,
	}
	if vmName != "" {
		response["vm_name"] = vmName
	}
	if len(interfaces) == 0 {
		response["note"] = "No guest network interfaces were reported. The QEMU guest agent may not be installed or running in the guest, or the VM may still be booting."
	}
	return marshalJSON(response)
}

func formatVMGuestInterfaces(raw []rawVMGuestInterface) []vmGuestInterface {
	interfaces := make([]vmGuestInterface, 0, len(raw))
	for _, r := range raw {
		iface := vmGuestInterface{
			Name: strings.TrimSpace(r.Name),
			MAC:  strings.TrimSpace(r.HardwareAddress),
		}
		for _, a := range r.IPAddresses {
			ip := strings.TrimSpace(a.IPAddress)
			if ip == "" {
				continue
			}
			formatted := ip
			if a.Prefix != nil && *a.Prefix >= 0 {
				formatted = fmt.Sprintf("%s/%d", ip, *a.Prefix)
			}
			switch strings.ToUpper(strings.TrimSpace(a.IPAddressType)) {
			case "IPV4":
				iface.IPv4 = append(iface.IPv4, formatted)
			case "IPV6":
				iface.IPv6 = append(iface.IPv6, formatted)
			default:
				if addr, err := netip.ParseAddr(ip); err == nil {
					if addr.Is4() {
						iface.IPv4 = append(iface.IPv4, formatted)
					} else {
						iface.IPv6 = append(iface.IPv6, formatted)
					}
				} else {
					iface.Other = append(iface.Other, formatted)
				}
			}
		}
		if iface.Name == "" && iface.MAC == "" && len(iface.IPv4) == 0 && len(iface.IPv6) == 0 && len(iface.Other) == 0 {
			continue
		}
		interfaces = append(interfaces, iface)
	}
	sort.Slice(interfaces, func(i, j int) bool {
		return interfaces[i].Name < interfaces[j].Name
	})
	return interfaces
}

func formatVMLabel(name string, id int) string {
	if name != "" {
		return fmt.Sprintf("%q (id %d)", name, id)
	}
	return fmt.Sprintf("id %d", id)
}

func isMethodNotFoundErr(err error) bool {
	var apiErr *truenas.APICallError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code == -32601 {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "method not found") || strings.Contains(msg, "method does not exist")
}

// needsNewerTrueNASError is the error for a tool whose middleware method the
// connected TrueNAS build does not serve (method not found).
func needsNewerTrueNASError(tool, method string) error {
	return fmt.Errorf("%s requires TrueNAS 28.0 or later; %s is not available on this TrueNAS release, so nothing was executed", tool, method)
}

// isGuestAgentErr reports whether err is the middleware's error for a QEMU
// guest agent that is not installed, not running or not answering.
func isGuestAgentErr(err error) bool {
	var apiErr *truenas.APICallError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "guest agent")
}

func isVMMissingErr(err error) bool {
	if isENOENT(err) {
		return true
	}
	var apiErr *truenas.APICallError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "match not found")
}

func isVMNotRunningErr(err error) bool {
	var apiErr *truenas.APICallError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "is not running")
}
