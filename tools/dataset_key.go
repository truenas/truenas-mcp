package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// ExportedKeyField is the one top-level field of an export_dataset_key
// result that carries the exported encryption key. The stdio handler keeps
// exactly this field out of output redaction for that tool (see
// redact.ToolOutputKeeping); everything else, including every other tool,
// is redacted as usual.
const ExportedKeyField = "key"

// Job polling for pool.dataset.export_key. Variables so tests can shorten
// them.
var (
	exportKeyPollInterval = 500 * time.Millisecond
	exportKeyTimeout      = 60 * time.Second
)

const exportKeyWarning = "SECURITY: this is the dataset's encryption key. Anyone who has it can unlock the dataset. " +
	"The key is now part of this conversation: it is in the model's context and may be stored in client logs and transcripts. " +
	"Copy it to an offline, access-controlled location (for example a password manager or an encrypted offline backup), " +
	"do not paste it anywhere else, and consider the conversation sensitive. The server never logs it and will not repeat it unless you call this tool again."

func (r *Registry) registerDatasetKeyTools() {
	r.tools["export_dataset_key"] = Tool{
		Definition: mcp.Tool{
			Name: "export_dataset_key",
			Description: "Reveal the stored encryption key of ONE key-encrypted dataset (pool.dataset.export_key). " +
				"This hands secret key material to the AI client: the key appears only in this tool's response, but it then sits in the conversation, " +
				"the model's context and any client-side logs or transcripts. Use it only when the user explicitly wants a backup copy of the key.\n\n" +
				"**WIZARD GUIDANCE FOR LLM:**\n" +
				"1. Only call this when the user explicitly asks to see or back up a dataset's encryption key. Never call it speculatively.\n" +
				"2. Call with dry_run=true (or without confirm) first; it checks the dataset and previews what will happen. Tell the user the key will be shown in the chat and ask for explicit approval.\n" +
				"3. Only after approval, call with confirm=true. Show the key once, repeat the storage warning from the response, and do not repeat the key again unless asked.\n" +
				"4. Works only for a dataset that is its own encryption root and uses a key (generate_key or key). Datasets using a passphrase cannot be exported: TrueNAS does not store passphrases. A child dataset that inherits encryption has no key of its own; export its encryption root instead.\n" +
				"5. Requires an API key with write access to datasets (DATASET_WRITE). Not available in read-only mode.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dataset": map[string]interface{}{
						"type":        "string",
						"description": "Full dataset path of the encryption root, for example 'tank/secure'. Must be encrypted with a key (not a passphrase).",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Check the dataset and preview the export without revealing the key (default: false). The key is never returned in a dry run.",
						"default":     false,
					},
				},
				"required": []string{"dataset"},
			},
		},
		Handler: handleExportDatasetKey,
	}
}

// datasetKeyInfo is what the export tool needs to know about a dataset.
type datasetKeyInfo struct {
	id             string
	encryptionRoot string
	keyFormat      string // lower case: hex, raw, passphrase, or ""
	locked         bool
}

func handleExportDatasetKey(client *truenas.Client, args map[string]interface{}) (string, error) {
	dataset, _ := args["dataset"].(string)
	if err := validateDatasetName(dataset); err != nil {
		return "", err
	}

	info, err := lookupDatasetKeyInfo(client, dataset)
	if err != nil {
		return "", err
	}

	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return marshalJSON(exportKeyPreview(info))
	}

	key, err := runExportKey(client, dataset)
	if err != nil {
		return "", err
	}

	// The key goes into exactly one place: this response.
	return marshalJSON(map[string]interface{}{
		"status":         "exported",
		"dataset":        info.id,
		"key_format":     info.keyFormat,
		ExportedKeyField: key,
		"warning":        exportKeyWarning,
	})
}

// lookupDatasetKeyInfo reads the dataset and refuses everything that has no
// exportable key, with a specific reason for each case.
func lookupDatasetKeyInfo(client *truenas.Client, dataset string) (*datasetKeyInfo, error) {
	result, err := client.Call("pool.dataset.query",
		[]interface{}{[]interface{}{"id", "=", dataset}},
		map[string]interface{}{"extra": map[string]interface{}{
			"retrieve_children": false,
			"properties":        []interface{}{"encryption", "keystatus", "keyformat", "encryptionroot"},
		}},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to look up dataset %q: %w", dataset, err)
	}
	var datasets []map[string]interface{}
	if err := json.Unmarshal(result, &datasets); err != nil {
		return nil, fmt.Errorf("failed to parse dataset: %w", err)
	}
	if len(datasets) == 0 {
		return nil, fmt.Errorf("dataset %q not found", dataset)
	}
	ds := datasets[0]

	if encrypted, _ := ds["encrypted"].(bool); !encrypted {
		return nil, fmt.Errorf("dataset %q is not encrypted, so it has no encryption key to export", dataset)
	}
	info := &datasetKeyInfo{id: dataset}
	info.encryptionRoot, _ = ds["encryption_root"].(string)
	info.locked, _ = ds["locked"].(bool)
	if kf, ok := ds["key_format"].(map[string]interface{}); ok {
		for _, f := range []string{"value", "parsed", "rawvalue"} {
			if v, ok := kf[f].(string); ok && v != "" {
				info.keyFormat = strings.ToLower(v)
				break
			}
		}
	}

	if info.encryptionRoot != "" && info.encryptionRoot != dataset {
		return nil, fmt.Errorf("dataset %q does not have an encryption key of its own: it inherits encryption from %q. "+
			"Export the key of that encryption root instead", dataset, info.encryptionRoot)
	}
	if info.keyFormat == "passphrase" {
		return nil, fmt.Errorf("dataset %q is encrypted with a passphrase. TrueNAS does not store passphrases, "+
			"so there is no key to export; the passphrase can only come from whoever set it", dataset)
	}
	return info, nil
}

func exportKeyPreview(info *datasetKeyInfo) *DryRunResult {
	state := map[string]interface{}{
		"dataset":         info.id,
		"encryption_root": info.encryptionRoot,
		"key_format":      info.keyFormat,
		"locked":          info.locked,
	}
	return &DryRunResult{
		Tool:         "export_dataset_key",
		CurrentState: state,
		PlannedActions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Export the stored encryption key of dataset %q and return it in this tool's response", info.id),
			Operation:   "export",
			Target:      "pool.dataset.export_key",
		}},
		Warnings: []string{
			fmt.Sprintf("The encryption key of %q WILL BE REVEALED in the response and will then be part of this conversation (the model's context and any client logs or transcripts).", info.id),
			"Nothing is changed on the system, and no key is returned in this preview.",
			"Only run this with explicit user approval, and store the key offline afterwards.",
		},
	}
}

// runExportKey runs pool.dataset.export_key as a job and returns its result.
// The key is never placed in an error, log line or any other return value.
func runExportKey(client *truenas.Client, dataset string) (string, error) {
	raw, err := client.Call("pool.dataset.export_key", dataset, false)
	if err != nil {
		return "", fmt.Errorf("failed to start the key export for %q: %w", dataset, err)
	}
	var jobID json.Number
	if err := json.Unmarshal(raw, &jobID); err != nil {
		return "", fmt.Errorf("unexpected response starting the key export for %q", dataset)
	}

	deadline := time.Now().Add(exportKeyTimeout)
	for {
		jobs, err := client.Call("core.get_jobs", []interface{}{[]interface{}{"id", "=", jobID}})
		if err != nil {
			return "", fmt.Errorf("failed to read the key export job %s: %w", jobID, err)
		}
		var list []map[string]interface{}
		if err := json.Unmarshal(jobs, &list); err != nil || len(list) != 1 {
			return "", fmt.Errorf("could not read the key export job %s", jobID)
		}
		job := list[0]
		switch state, _ := job["state"].(string); state {
		case "SUCCESS":
			key, ok := job["result"].(string)
			if !ok || key == "" {
				return "", fmt.Errorf("TrueNAS returned no key for dataset %q (it may not have a stored key)", dataset)
			}
			return key, nil
		case "FAILED", "ABORTED":
			msg, _ := job["error"].(string)
			if msg == "" {
				msg = strings.ToLower(state)
			}
			return "", fmt.Errorf("key export for %q failed: %s", dataset, redact.Text(msg))
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("key export for %q did not finish within %s (job %s)", dataset, exportKeyTimeout, jobID)
		}
		time.Sleep(exportKeyPollInterval)
	}
}
