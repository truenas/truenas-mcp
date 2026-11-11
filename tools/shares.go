package tools

// Share query tool and its handler (query_shares). create_smb_share and
// create_nfs_share are registered from smb.go and nfs.go.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerShareTools() {
	// Shares query
	r.tools["query_shares"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_shares",
			Description: "Query SMB and NFS shares configuration. Returns allow-listed share fields (including hostsallow/hostsdeny and NFS maps); extra middleware keys such as auxsmbconf are omitted.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"smb", "nfs", "all"},
						"description": "Type of shares to query (default: all)",
						"default":     "all",
					},
				},
			},
		},
		Handler: handleQueryShares,
	}
}

func handleQueryShares(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	shareType := "all"
	if st, ok := args["share_type"].(string); ok && st != "" {
		shareType = st
	}

	response := make(map[string]interface{})

	if shareType == "smb" || shareType == "all" {
		result, err := client.Call(ctx, "sharing.smb.query", []interface{}{}, map[string]interface{}{"select": smbShareQuerySelect})
		if err != nil {
			return "", fmt.Errorf("failed to query SMB shares: %w", err)
		}

		var smbShares []map[string]interface{}
		if err := json.Unmarshal(result, &smbShares); err != nil {
			return "", fmt.Errorf("failed to parse SMB shares: %w", err)
		}
		simplified := make([]map[string]interface{}, 0, len(smbShares))
		for _, share := range smbShares {
			simplified = append(simplified, simplifySMBShare(share))
		}
		response["smb_shares"] = simplified
	}

	if shareType == "nfs" || shareType == "all" {
		result, err := client.Call(ctx, "sharing.nfs.query", []interface{}{}, map[string]interface{}{"select": nfsShareQuerySelect})
		if err != nil {
			return "", fmt.Errorf("failed to query NFS shares: %w", err)
		}

		var nfsShares []map[string]interface{}
		if err := json.Unmarshal(result, &nfsShares); err != nil {
			return "", fmt.Errorf("failed to parse NFS shares: %w", err)
		}
		simplified := make([]map[string]interface{}, 0, len(nfsShares))
		for _, share := range nfsShares {
			simplified = append(simplified, simplifyNFSShare(share))
		}
		response["nfs_shares"] = simplified
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}
