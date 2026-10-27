package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

const testExportedKey = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

func init() {
	exportKeyPollInterval = time.Millisecond
	exportKeyTimeout = 2 * time.Second
}

// keyFake serves one dataset and a successful key export job.
type keyFake struct {
	dataset map[string]interface{} // nil: the dataset does not exist
	state   string                 // job state to report; default SUCCESS
	result  interface{}            // job result; default testExportedKey
	jobErr  string
	polls   int
	mu      sync.Mutex
}

func (k *keyFake) middleware() *fakeMiddleware {
	return &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		k.mu.Lock()
		defer k.mu.Unlock()
		switch method {
		case "pool.dataset.query":
			if k.dataset == nil {
				return []interface{}{}, nil, true
			}
			return []interface{}{k.dataset}, nil, true
		case "pool.dataset.export_key":
			return 77, nil, true
		case "core.get_jobs":
			k.polls++
			state := k.state
			if state == "" {
				state = "SUCCESS"
			}
			var result interface{} = testExportedKey
			if k.result != nil {
				result = k.result
			}
			job := map[string]interface{}{"id": 77, "method": "pool.dataset.export_key", "state": state, "result": nil, "error": nil}
			if state == "SUCCESS" {
				job["result"] = result
			}
			if k.jobErr != "" {
				job["error"] = k.jobErr
			}
			return []interface{}{job}, nil, true
		}
		return nil, nil, false
	}}
}

func hexKeyDataset(name string) map[string]interface{} {
	return map[string]interface{}{
		"id": name, "name": name, "encrypted": true, "encryption_root": name, "locked": false,
		"key_format": map[string]interface{}{"parsed": "hex", "rawvalue": "hex", "value": "HEX", "source": "LOCAL"},
	}
}

func exportKeyRegistry(t *testing.T, k *keyFake) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := k.middleware()
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func decodeKeyResult(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return m
}

func TestExportDatasetKeyIsConfirmGated(t *testing.T) {
	r, fake := exportKeyRegistry(t, &keyFake{dataset: hexKeyDataset("tank/secure")})

	// No confirm: a preview that names the dataset and says the key will be revealed.
	out, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeKeyResult(t, out)
	if m["status"] != "confirmation_required" || m["executed"] != false {
		t.Errorf("reply = %v", m)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "reveals a secret") || !strings.Contains(msg, "NOT executed") {
		t.Errorf("message = %q", msg)
	}
	if !strings.Contains(out, "tank/secure") || !strings.Contains(out, "WILL BE REVEALED") {
		t.Errorf("preview does not name the dataset or warn: %s", out)
	}
	if strings.Contains(out, testExportedKey) || len(fake.called("pool.dataset.export_key")) != 0 {
		t.Error("the key must not be fetched or shown without confirm")
	}

	// confirm=false is the same as no confirm.
	out, err = r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": false})
	if err != nil || strings.Contains(out, testExportedKey) || len(fake.called("pool.dataset.export_key")) != 0 {
		t.Errorf("confirm=false exported: %v %s", err, out)
	}

	// dry_run (with or without confirm) previews and never exports.
	for _, args := range []map[string]interface{}{
		{"dataset": "tank/secure", "dry_run": true},
		{"dataset": "tank/secure", "dry_run": true, "confirm": true},
	} {
		out, err = r.CallTool("export_dataset_key", args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, testExportedKey) || len(fake.called("pool.dataset.export_key")) != 0 {
			t.Fatalf("dry run exported the key: %s", out)
		}
		if !strings.Contains(out, "WILL BE REVEALED") || !strings.Contains(out, "tank/secure") {
			t.Errorf("preview: %s", out)
		}
	}

	// confirm=true exports.
	out, err = r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeKeyResult(t, out)[ExportedKeyField]; got != testExportedKey {
		t.Errorf("key = %v", got)
	}
	if n := len(fake.called("pool.dataset.export_key")); n != 1 {
		t.Errorf("export_key called %d times", n)
	}
}

func TestExportDatasetKeyResponseShape(t *testing.T) {
	r, fake := exportKeyRegistry(t, &keyFake{dataset: hexKeyDataset("tank/secure")})
	out, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeKeyResult(t, out)
	for field, want := range map[string]interface{}{"status": "exported", "dataset": "tank/secure", "key_format": "hex", "key": testExportedKey} {
		if m[field] != want {
			t.Errorf("%s = %v, want %v", field, m[field], want)
		}
	}
	w, _ := m["warning"].(string)
	for _, s := range []string{"offline", "conversation", "model's context", "logs"} {
		if !strings.Contains(w, s) {
			t.Errorf("warning missing %q: %s", s, w)
		}
	}
	if strings.Count(out, testExportedKey) != 1 {
		t.Errorf("the key must appear exactly once in the response")
	}
	// The middleware call asks for the string result, not the download.
	calls := fake.called("pool.dataset.export_key")
	if len(calls) != 1 || string(calls[0].Params) != `["tank/secure",false]` {
		t.Errorf("export_key params = %v", calls)
	}
	if len(m) != 5 {
		t.Errorf("unexpected fields in the response: %v", m)
	}
}

func TestExportDatasetKeyRejections(t *testing.T) {
	passphrase := hexKeyDataset("tank/pp")
	passphrase["key_format"] = map[string]interface{}{"parsed": "passphrase", "rawvalue": "passphrase", "value": "PASSPHRASE"}
	child := hexKeyDataset("tank/secure/child")
	child["encryption_root"] = "tank/secure"
	plain := map[string]interface{}{"id": "tank/plain", "encrypted": false, "encryption_root": nil, "key_format": map[string]interface{}{"parsed": nil, "value": nil}}

	for _, tt := range []struct {
		name    string
		dataset string
		ds      map[string]interface{}
		args    map[string]interface{}
		want    string
	}{
		{"passphrase", "tank/pp", passphrase, nil, "TrueNAS does not store passphrases"},
		{"not encrypted", "tank/plain", plain, nil, "is not encrypted"},
		{"inherits encryption", "tank/secure/child", child, nil, `inherits encryption from "tank/secure"`},
		{"not found", "tank/missing", nil, nil, "not found"},
		{"invalid name", "no-slash", nil, nil, "must include pool name"},
		{"missing dataset", "", nil, nil, "cannot be empty"},
	} {
		for _, mode := range []string{"confirm", "dry_run", "unconfirmed"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				r, fake := exportKeyRegistry(t, &keyFake{dataset: tt.ds})
				args := map[string]interface{}{"dataset": tt.dataset}
				switch mode {
				case "confirm":
					args["confirm"] = true
				case "dry_run":
					args["dry_run"] = true
				}
				out, err := r.CallTool("export_dataset_key", args)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("err = %v, out = %s; want %q", err, out, tt.want)
				}
				if len(fake.called("pool.dataset.export_key")) != 0 {
					t.Error("export_key must not be called for a rejected dataset")
				}
				if strings.Contains(out+err.Error(), testExportedKey) {
					t.Error("key present")
				}
			})
		}
	}
}

func TestExportDatasetKeyAcceptsLockedAndRawKeyDatasets(t *testing.T) {
	for _, format := range []string{"HEX", "RAW"} {
		ds := hexKeyDataset("tank/secure")
		ds["locked"] = true
		ds["key_format"] = map[string]interface{}{"value": format}
		r, _ := exportKeyRegistry(t, &keyFake{dataset: ds})
		out, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if m := decodeKeyResult(t, out); m["key_format"] != strings.ToLower(format) || m[ExportedKeyField] != testExportedKey {
			t.Errorf("%s: %v", format, m)
		}
	}
}

func TestExportDatasetKeyJobFailures(t *testing.T) {
	for name, k := range map[string]*keyFake{
		"failed":         {dataset: hexKeyDataset("tank/secure"), state: "FAILED", jobErr: "[EINVAL] Specified dataset does not have its own encryption key."},
		"aborted":        {dataset: hexKeyDataset("tank/secure"), state: "ABORTED"},
		"empty result":   {dataset: hexKeyDataset("tank/secure"), result: ""},
		"null result":    {dataset: hexKeyDataset("tank/secure"), result: false},
		"never finishes": {dataset: hexKeyDataset("tank/secure"), state: "RUNNING"},
	} {
		t.Run(name, func(t *testing.T) {
			old := exportKeyTimeout
			exportKeyTimeout = 50 * time.Millisecond
			defer func() { exportKeyTimeout = old }()
			r, _ := exportKeyRegistry(t, k)
			out, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
			if err == nil {
				t.Fatalf("expected an error, got %s", out)
			}
			if strings.Contains(err.Error(), testExportedKey) {
				t.Errorf("error contains the key: %v", err)
			}
		})
	}
}

// A failed job whose error text quotes secrets is redacted like other errors.
func TestExportDatasetKeyJobErrorIsRedacted(t *testing.T) {
	k := &keyFake{dataset: hexKeyDataset("tank/secure"), state: "FAILED", jobErr: "boom password=hunter2hunter2"}
	r, _ := exportKeyRegistry(t, k)
	_, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
	if err == nil || strings.Contains(err.Error(), "hunter2hunter2") {
		t.Errorf("err = %v", err)
	}
}

// The key must never reach the log, even with debug logging on (which writes
// request and response bodies), on success or on failure.
func TestExportDatasetKeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &buf})
	truenas.SetDebugLogging(true)
	defer func() {
		truenas.SetDebugLogging(false)
		log.SetOutput(os.Stderr)
	}()

	r, fake := exportKeyRegistry(t, &keyFake{dataset: hexKeyDataset("tank/secure")})
	out, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true})
	if err != nil || !strings.Contains(out, testExportedKey) {
		t.Fatalf("export failed: %v %s", err, out)
	}
	if _, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure"}); err != nil {
		t.Fatal(err)
	}
	// Reading the job back with the generic jobs tool must not reveal it
	// either (the stdio handler redacts every result of that tool).
	jobs, err := r.CallTool("query_jobs", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if shown := redact.ToolOutput(jobs); strings.Contains(shown, testExportedKey) {
		t.Errorf("query_jobs would expose the key: %s", shown)
	}

	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "core.get_jobs") {
		t.Fatalf("test setup: expected request logging, got:\n%s", logged)
	}
	if strings.Contains(logged, testExportedKey) {
		t.Errorf("the key was logged:\n%s", logged)
	}
	// Nothing sent to TrueNAS carries it either.
	for _, c := range fake.calls {
		if strings.Contains(string(c.Params), testExportedKey) {
			t.Errorf("the key appears in request params of %s", c.Method)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestExportDatasetKeyClassification(t *testing.T) {
	s := safetyOf("export_dataset_key")
	if s.Access != Write || !s.RevealsSecret {
		t.Fatalf("safety = %+v, want a write tool that reveals a secret", s)
	}
	r := NewRegistry(nil, nil)
	tool := r.tools["export_dataset_key"]
	props := tool.Definition.InputSchema["properties"].(map[string]interface{})
	for _, p := range []string{"dataset", "dry_run", "confirm"} {
		if _, ok := props[p]; !ok {
			t.Errorf("missing parameter %q", p)
		}
	}
	if !strings.Contains(tool.Definition.Description, "REVEALS A SECRET: requires confirm=true") || strings.Contains(tool.Definition.Description, "DESTRUCTIVE") {
		t.Errorf("description: %s", tool.Definition.Description)
	}
	if a := tool.Definition.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint {
		t.Errorf("annotations = %+v", a)
	}
	if !r.RevealsSecret("export_dataset_key") || r.RevealsSecret("query_datasets") || r.RevealsSecret("no_such_tool") {
		t.Error("RevealsSecret reports the wrong tools")
	}
	// Only this tool is exempted from output redaction.
	for name, s := range toolSafety {
		if s.RevealsSecret && name != "export_dataset_key" {
			t.Errorf("%s unexpectedly reveals secrets", name)
		}
	}
}

func TestExportDatasetKeyPolicy(t *testing.T) {
	r := NewRegistry(nil, nil)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("read-only mode must refuse the tool, got %v", err)
	}
	for _, tool := range r.ListTools() {
		if tool.Name == "export_dataset_key" {
			t.Error("read-only mode must hide the tool")
		}
	}
	r = NewRegistry(nil, nil)
	if err := r.SetPolicy(Policy{Deny: []string{"export_dataset_key"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool("export_dataset_key", map[string]interface{}{"dataset": "tank/secure", "confirm": true}); err == nil || !strings.Contains(err.Error(), "DENY_TOOLS") {
		t.Errorf("deny list must refuse the tool, got %v", err)
	}
}
