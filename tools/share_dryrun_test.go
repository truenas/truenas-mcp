package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func shareDryRunRegistry(t *testing.T, results map[string]interface{}) *Registry {
	t.Helper()
	if results == nil {
		results = map[string]interface{}{}
	}
	if _, ok := results["system.info"]; !ok {
		results["system.info"] = map[string]interface{}{
			"hostname": "nas.example.invalid",
			"version":  "25.10.0",
		}
	}
	client, _ := newFakeClientWith(t, results, nil)
	return NewRegistry(client, nil)
}

func callShareDryRun(t *testing.T, r *Registry, tool string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	args["dry_run"] = true
	out, err := r.CallTool(tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("%s: parse %v; body=%s", tool, err, out)
	}
	if resp["dry_run"] != true {
		t.Fatalf("%s: not a dry run: %s", tool, out)
	}
	return resp
}

func warningStrings(resp map[string]interface{}) []string {
	raw, _ := resp["security_warnings"].([]interface{})
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		if s, ok := w.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// TestShareDryRunWarnings is the regression for wrong or missing share
// dry-run warnings (SMB hostsallow path, NFS maproot, directory-service
// client, and hard-coded hostname).
func TestShareDryRunWarnings(t *testing.T) {
	smbBase := map[string]interface{}{"name": "media", "path": "/mnt/tank/media"}
	nfsBase := map[string]interface{}{"path": "/mnt/tank/media"}

	t.Run("SMB hostsallow set does not warn about any network host", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		// browsable is set explicitly so the warning path is taken even
		// on unfixed code; the bug is looking at payload["hostsallow"]
		// instead of payload["options"]["hostsallow"].
		resp := callShareDryRun(t, r, "create_smb_share", mergeShareArgs(smbBase, map[string]interface{}{
			"browsable":  true,
			"hostsallow": []interface{}{"192.168.1.0/24"},
		}))
		if hasWarning(warningStrings(resp), "any network host") {
			t.Errorf("hostsallow was set, but got any-host warning: %v", resp["security_warnings"])
		}
	})

	t.Run("SMB omitted browsable and no hostsallow warns", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_smb_share", mergeShareArgs(smbBase, nil))
		if _, ok := resp["payload"].(map[string]interface{})["browsable"]; ok {
			t.Errorf("payload should omit browsable when the caller did: %v", resp["payload"])
		}
		if !hasWarning(warningStrings(resp), "any network host") {
			t.Errorf("missing any-host warning: %v", resp["security_warnings"])
		}
	})

	t.Run("NFS without maproot does not claim root access", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, nil))
		if hasWarning(warningStrings(resp), "root access") {
			t.Errorf("default export is root_squash, but got root-access warning: %v", resp["security_warnings"])
		}
	})

	t.Run("NFS maproot_user root alone is root_squash with uid 0", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, map[string]interface{}{
			"maproot_user": "root",
		}))
		ws := warningStrings(resp)
		if hasWarning(ws, "no_root_squash") {
			t.Errorf("maproot_user=root alone is not no_root_squash: %v", ws)
		}
		if !hasWarning(ws, "uid 0") || !hasWarning(ws, "root_squash") {
			t.Errorf("maproot_user=root alone should mention uid 0 and root_squash: %v", ws)
		}
	})

	t.Run("NFS maproot_group root alone is gid 0 only", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, map[string]interface{}{
			"maproot_group": "root",
		}))
		ws := warningStrings(resp)
		if hasWarning(ws, "no_root_squash") || hasWarning(ws, "root access") {
			t.Errorf("maproot_group=root alone is not no_root_squash or root access: %v", ws)
		}
		if !hasWarning(ws, "gid 0") || !hasWarning(ws, "root_squash") {
			t.Errorf("maproot_group=root alone should mention gid 0 and root_squash: %v", ws)
		}
	})

	t.Run("NFS maproot_user alice is mapped to alice not nobody", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, map[string]interface{}{
			"maproot_user": "alice",
		}))
		ws := warningStrings(resp)
		if !hasWarning(ws, "alice") {
			t.Errorf("maproot_user=alice should name alice: %v", ws)
		}
		if hasWarning(ws, "nobody") {
			t.Errorf("maproot_user=alice is not squashed to nobody: %v", ws)
		}
		if hasWarning(ws, "no_root_squash") {
			t.Errorf("maproot_user=alice is not no_root_squash: %v", ws)
		}
	})

	t.Run("NFS maproot_user and group root is no_root_squash", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, map[string]interface{}{
			"maproot_user":  "root",
			"maproot_group": "root",
		}))
		ws := warningStrings(resp)
		if !hasWarning(ws, "no_root_squash") {
			t.Errorf("maproot user and group root should be no_root_squash: %v", ws)
		}
	})

	t.Run("NFS mapall names the mapped account", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		resp := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, map[string]interface{}{
			"mapall_user":  "nobody",
			"mapall_group": "nogroup",
		}))
		ws := warningStrings(resp)
		if !hasWarning(ws, "all_squash") || !hasWarning(ws, "nobody") || !hasWarning(ws, "nogroup") {
			t.Errorf("mapall should name nobody/nogroup and all_squash: %v", ws)
		}
		if hasWarning(ws, "no_root_squash") {
			t.Errorf("mapall is not no_root_squash: %v", ws)
		}
	})

	t.Run("IPA enabled notice appears on both dry runs", func(t *testing.T) {
		r := shareDryRunRegistry(t, map[string]interface{}{
			"directoryservices.status": map[string]interface{}{
				"type": "IPA", "status": "HEALTHY", "status_msg": nil,
			},
		})
		for _, tool := range []string{"create_smb_share", "create_nfs_share"} {
			args := mergeShareArgs(smbBase, nil)
			if tool == "create_nfs_share" {
				args = mergeShareArgs(nfsBase, nil)
			}
			resp := callShareDryRun(t, r, tool, args)
			if !hasWarning(warningStrings(resp), "IPA is enabled") {
				t.Errorf("%s: missing IPA enabled notice: %v", tool, resp["security_warnings"])
			}
		}
	})

	t.Run("directory service warnings appear on both dry runs", func(t *testing.T) {
		r := shareDryRunRegistry(t, map[string]interface{}{
			"directoryservices.status": map[string]interface{}{
				"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil,
			},
		})
		for _, tool := range []string{"create_smb_share", "create_nfs_share"} {
			args := mergeShareArgs(smbBase, nil)
			if tool == "create_nfs_share" {
				args = mergeShareArgs(nfsBase, nil)
			}
			resp := callShareDryRun(t, r, tool, args)
			if !hasWarning(warningStrings(resp), "Active Directory is enabled") {
				t.Errorf("%s: missing directory-service warning: %v", tool, resp["security_warnings"])
			}
		}
	})

	t.Run("network_path and mount_example use the NAS hostname", func(t *testing.T) {
		r := shareDryRunRegistry(t, nil)
		smb := callShareDryRun(t, r, "create_smb_share", mergeShareArgs(smbBase, nil))
		path, _ := smb["network_path"].(string)
		if path != `\\nas.example.invalid\media` {
			t.Errorf("network_path = %q, want \\\\nas.example.invalid\\media", path)
		}
		if strings.Contains(path, "truenas") && !strings.Contains(path, "nas.example.invalid") {
			t.Errorf("network_path used the hard-coded host: %q", path)
		}
		nfs := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, nil))
		ex, _ := nfs["mount_example"].(string)
		if !strings.Contains(ex, "nas.example.invalid:/mnt/tank/media") {
			t.Errorf("mount_example = %q, want the fake hostname", ex)
		}
		if strings.Contains(ex, "truenas:") {
			t.Errorf("mount_example used the hard-coded host: %q", ex)
		}
	})
}

func TestShareHostnameUnavailable(t *testing.T) {
	smbBase := map[string]interface{}{"name": "media", "path": "/mnt/tank/media"}
	nfsBase := map[string]interface{}{"path": "/mnt/tank/media"}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"sharing.smb.create": map[string]interface{}{"id": 1.0, "name": "media", "path": "/mnt/tank/media", "enabled": true},
			"sharing.nfs.create": map[string]interface{}{"id": 2.0, "path": "/mnt/tank/media", "enabled": true},
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "system.info" {
				return nil, map[string]interface{}{"code": 1, "message": "unavailable"}, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	r := NewRegistry(client, nil)

	assertPlaceholder := func(t *testing.T, tool, field, value string, resp map[string]interface{}) {
		t.Helper()
		if !strings.Contains(value, "<hostname unavailable>") {
			t.Errorf("%s %s = %q, want the hostname placeholder", tool, field, value)
		}
		if strings.Contains(value, "truenas") && !strings.Contains(value, "unavailable") {
			t.Errorf("%s %s used a silent truenas fallback: %q", tool, field, value)
		}
		note, _ := resp["hostname_note"].(string)
		if note == "" || !strings.Contains(strings.ToLower(note), "unavailable") {
			t.Errorf("%s missing hostname_note marking the placeholder: %v", tool, resp["hostname_note"])
		}
	}

	t.Run("dry-run preview", func(t *testing.T) {
		smb := callShareDryRun(t, r, "create_smb_share", mergeShareArgs(smbBase, nil))
		path, _ := smb["network_path"].(string)
		assertPlaceholder(t, "create_smb_share", "network_path", path, smb)
		nfs := callShareDryRun(t, r, "create_nfs_share", mergeShareArgs(nfsBase, nil))
		ex, _ := nfs["mount_example"].(string)
		assertPlaceholder(t, "create_nfs_share", "mount_example", ex, nfs)
	})

	t.Run("create result", func(t *testing.T) {
		out, err := r.CallTool("create_smb_share", mergeShareArgs(smbBase, nil))
		if err != nil {
			t.Fatalf("create_smb_share: %v", err)
		}
		var smb map[string]interface{}
		if err := json.Unmarshal([]byte(out), &smb); err != nil {
			t.Fatalf("parse: %v; body=%s", err, out)
		}
		path, _ := smb["network_path"].(string)
		assertPlaceholder(t, "create_smb_share", "network_path", path, smb)

		out, err = r.CallTool("create_nfs_share", mergeShareArgs(nfsBase, nil))
		if err != nil {
			t.Fatalf("create_nfs_share: %v", err)
		}
		var nfs map[string]interface{}
		if err := json.Unmarshal([]byte(out), &nfs); err != nil {
			t.Fatalf("parse: %v; body=%s", err, out)
		}
		ex, _ := nfs["mount_example"].(string)
		assertPlaceholder(t, "create_nfs_share", "mount_example", ex, nfs)
	})
}

func mergeShareArgs(base, extra map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
