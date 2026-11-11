package tools

// Storage pool query tool and its handler (query_pools).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerPoolTools() {
	// Storage pools query
	r.tools["query_pools"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_pools",
			Description: "Query storage pools with their status, capacity, and health information. Returns allow-listed pool fields, RAIDZ expansion progress, a compact topology (type, path, name, guid, status, disk, device, and trimmed read/write/checksum error stats), and a trimmed scrub/resilver scan; it does not pass through the raw middleware record.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleQueryPools,
	}
}

func handleQueryPools(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call(ctx, "pool.query", []interface{}{}, map[string]interface{}{
		"select": poolQuerySelect,
		"extra":  map[string]interface{}{"is_upgraded": true},
	})
	if err != nil {
		return "", err
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(result, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools: %w", err)
	}

	simplified := make([]map[string]interface{}, 0, len(pools))
	for _, pool := range pools {
		simplified = append(simplified, simplifyPool(pool))
	}

	formatted, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}
