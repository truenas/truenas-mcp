package tools

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const (
	testPassphrase = "correct-horse-battery-staple"
	testKeyHex     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func encFakeResults() map[string]interface{} {
	return map[string]interface{}{
		"pool.dataset.create": map[string]interface{}{
			"id": "tank/secure", "name": "tank/secure", "type": "FILESYSTEM", "pool": "tank",
			"encrypted": true, "key_loaded": true,
		},
	}
}

// createPayload returns the single pool.dataset.create payload the fake saw.
func createPayload(t *testing.T, fake *fakeMiddleware) map[string]interface{} {
	t.Helper()
	calls := fake.called("pool.dataset.create")
	if len(calls) != 1 {
		t.Fatalf("pool.dataset.create calls = %d, want 1", len(calls))
	}
	var params []map[string]interface{}
	if err := json.Unmarshal(calls[0].Params, &params); err != nil || len(params) != 1 {
		t.Fatalf("params = %s (%v)", calls[0].Params, err)
	}
	return params[0]
}

func TestCreateDatasetEncryptionPayloads(t *testing.T) {
	tests := []struct {
		name string
		opts map[string]interface{}
		want map[string]interface{}
	}{
		{"generate_key", map[string]interface{}{"generate_key": true}, map[string]interface{}{"generate_key": true}},
		{"passphrase", map[string]interface{}{"passphrase": testPassphrase}, map[string]interface{}{"passphrase": testPassphrase}},
		{"key", map[string]interface{}{"key": testKeyHex}, map[string]interface{}{"key": testKeyHex}},
		{
			"passphrase with pbkdf2iters",
			map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(1300000)},
			map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(1300000)},
		},
		{
			"generate_key false is not sent",
			map[string]interface{}{"generate_key": false, "passphrase": testPassphrase},
			map[string]interface{}{"passphrase": testPassphrase},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, encFakeResults(), nil)
			_, err := handleCreateDataset(client, map[string]interface{}{
				"name": "tank/secure", "encryption": true, "encryption_options": tt.opts,
			})
			if err != nil {
				t.Fatal(err)
			}
			p := createPayload(t, fake)
			if p["encryption"] != true {
				t.Errorf("encryption = %v, want true", p["encryption"])
			}
			if p["inherit_encryption"] != false {
				t.Errorf("inherit_encryption = %v, want false", p["inherit_encryption"])
			}
			got, _ := p["encryption_options"].(map[string]interface{})
			if len(got) != len(tt.want) {
				t.Fatalf("encryption_options = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("encryption_options[%s] = %v, want %v", k, got[k], v)
				}
			}
			if _, ok := got["algorithm"]; ok {
				t.Error("algorithm must never be sent")
			}
		})
	}
}

func TestCreateDatasetWithoutEncryptionUnchanged(t *testing.T) {
	client, fake := newFakeClientWith(t, encFakeResults(), nil)
	if _, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/plain", "encryption": false}); err != nil {
		t.Fatal(err)
	}
	p := createPayload(t, fake)
	for _, k := range []string{"encryption", "encryption_options", "inherit_encryption"} {
		if _, ok := p[k]; ok {
			t.Errorf("payload unexpectedly carries %s: %v", k, p)
		}
	}

	// inherit_encryption alone keeps working as before.
	client, fake = newFakeClientWith(t, encFakeResults(), nil)
	if _, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/plain", "inherit_encryption": true}); err != nil {
		t.Fatal(err)
	}
	if p := createPayload(t, fake); p["inherit_encryption"] != true {
		t.Errorf("inherit_encryption = %v, want true", p["inherit_encryption"])
	}

	// An empty encryption_options object is a no-op.
	client, fake = newFakeClientWith(t, encFakeResults(), nil)
	if _, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/plain", "encryption_options": map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if p := createPayload(t, fake); p["encryption_options"] != nil || p["encryption"] != nil {
		t.Errorf("empty options leaked into payload: %v", p)
	}
}

func TestCreateDatasetEncryptionValidation(t *testing.T) {
	gen := map[string]interface{}{"generate_key": true}
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"options without encryption", map[string]interface{}{"encryption_options": gen}, "requires encryption=true"},
		{"options with encryption false", map[string]interface{}{"encryption": false, "encryption_options": gen}, "requires encryption=true"},
		{"encryption not a bool", map[string]interface{}{"encryption": "yes", "encryption_options": gen}, "encryption must be a boolean"},
		{"options not an object", map[string]interface{}{"encryption": true, "encryption_options": "x"}, "encryption_options must be an object"},
		{"encryption without options", map[string]interface{}{"encryption": true}, "requires exactly one of"},
		{"inherit with encryption", map[string]interface{}{"encryption": true, "inherit_encryption": true, "encryption_options": gen}, "inherit_encryption must be false"},
		{"key and passphrase", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"key": testKeyHex, "passphrase": testPassphrase}}, "only one of"},
		{"generate_key and key", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "key": testKeyHex}}, "only one of"},
		{"short passphrase", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"passphrase": "short"}}, "at least 8 characters"},
		{"short key", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"key": "abcd"}}, "64 hexadecimal characters"},
		{"non-hex key", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"key": strings.Repeat("zz", 32)}}, "64 hexadecimal characters"},
		{"passphrase wrong type", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"passphrase": float64(12345678)}}, "passphrase must be a string"},
		{"generate_key wrong type", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": "true"}}, "generate_key must be a boolean"},
		{"algorithm", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "algorithm": "AES-256-GCM"}}, "algorithm is not configurable through this tool"},
		{"unknown field", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "bogus": 1}}, `unknown encryption_options field "bogus"`},
		{"unsafe unknown field name", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "pass word=hunter2": 1}}, "field with invalid characters"},
		{"pbkdf2iters without passphrase", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "pbkdf2iters": float64(1300000)}}, "only to passphrase"},
		{"pbkdf2iters too low", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(1000)}}, "pbkdf2iters must be an integer"},
		{"pbkdf2iters fractional", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": 1300000.5}}, "pbkdf2iters must be an integer"},
		{"pbkdf2iters wrong type", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": "many"}}, "pbkdf2iters must be an integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, encFakeResults(), nil)
			tt.args["name"] = "tank/secure"
			_, err := handleCreateDataset(client, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			for _, secret := range []string{testPassphrase, testKeyHex, "hunter2"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %v", err)
				}
			}
			if n := len(fake.called("pool.dataset.create")); n != 0 {
				t.Errorf("middleware called %d times despite validation error", n)
			}
		})
	}
}

func TestCreateDatasetDryRunMasksSecrets(t *testing.T) {
	for name, opts := range map[string]map[string]interface{}{
		"passphrase": {"passphrase": testPassphrase, "pbkdf2iters": float64(1300000)},
		"key":        {"key": testKeyHex},
		"generate":   {"generate_key": true},
	} {
		t.Run(name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, encFakeResults(), nil)
			out, err := handleCreateDataset(client, map[string]interface{}{
				"name": "tank/secure", "encryption": true, "encryption_options": opts, "dry_run": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, testPassphrase) || strings.Contains(out, testKeyHex) {
				t.Fatalf("dry run leaks a secret: %s", out)
			}
			var preview struct {
				Payload map[string]interface{} `json:"payload"`
			}
			if err := json.Unmarshal([]byte(out), &preview); err != nil {
				t.Fatal(err)
			}
			got := preview.Payload["encryption_options"].(map[string]interface{})
			if preview.Payload["encryption"] != true || preview.Payload["inherit_encryption"] != false {
				t.Errorf("preview payload = %v", preview.Payload)
			}
			for _, k := range []string{"passphrase", "key"} {
				if _, ok := opts[k]; ok && got[k] != "[REDACTED]" {
					t.Errorf("%s = %v, want masked", k, got[k])
				}
			}
			if _, ok := opts["generate_key"]; ok && got["generate_key"] != true {
				t.Errorf("generate_key = %v", got["generate_key"])
			}
			if _, ok := opts["pbkdf2iters"]; ok && got["pbkdf2iters"] != float64(1300000) {
				t.Errorf("pbkdf2iters = %v", got["pbkdf2iters"])
			}
			if n := len(fake.called("pool.dataset.create")); n != 0 {
				t.Errorf("dry run called the middleware %d times", n)
			}
		})
	}
}

func TestMaskDatasetPayloadDoesNotMutateInput(t *testing.T) {
	payload := map[string]interface{}{
		"name":               "tank/secure",
		"encryption_options": map[string]interface{}{"passphrase": testPassphrase},
	}
	masked := maskDatasetPayload(payload)
	if payload["encryption_options"].(map[string]interface{})["passphrase"] != testPassphrase {
		t.Error("input payload was modified; the real call would lose the passphrase")
	}
	if masked["encryption_options"].(map[string]interface{})["passphrase"] != "[REDACTED]" {
		t.Errorf("masked = %v", masked)
	}
}

func TestCreateDatasetSuccessResponseHasNoSecrets(t *testing.T) {
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"pool.dataset.create": map[string]interface{}{
			"id": "tank/secure", "name": "tank/secure", "type": "FILESYSTEM", "pool": "tank",
			"encrypted": true, "key_loaded": true,
		},
	}, nil)
	out, err := handleCreateDataset(client, map[string]interface{}{
		"name": "tank/secure", "encryption": true,
		"encryption_options": map[string]interface{}{"passphrase": testPassphrase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, testPassphrase) || !strings.Contains(out, `"encrypted": true`) {
		t.Errorf("response = %s", out)
	}
}

// A middleware error must not echo the passphrase or key back through the
// tool result, which includes the request parameters for debugging.
func TestCreateDatasetMiddlewareErrorMasksSecrets(t *testing.T) {
	for name, opts := range map[string]map[string]interface{}{
		"passphrase": {"passphrase": testPassphrase},
		"key":        {"key": testKeyHex},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method == "pool.dataset.create" {
					return nil, map[string]interface{}{"code": -32001, "message": "[EINVAL] pool.dataset.create: invalid request"}, true
				}
				return nil, nil, false
			}}
			client, _ := newFakeClientFrom(t, fake)
			_, err := handleCreateDataset(client, map[string]interface{}{
				"name": "tank/secure", "encryption": true, "encryption_options": opts,
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), testPassphrase) || strings.Contains(err.Error(), testKeyHex) {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestCreateDatasetEncryptionSchema(t *testing.T) {
	r := NewRegistry(nil, nil)
	schema := r.tools["create_dataset"].Definition.InputSchema
	props := schema["properties"].(map[string]interface{})
	enc, ok := props["encryption"].(map[string]interface{})
	if !ok || enc["type"] != "boolean" {
		t.Fatalf("encryption schema = %v", props["encryption"])
	}
	opts, ok := props["encryption_options"].(map[string]interface{})
	if !ok || opts["type"] != "object" {
		t.Fatalf("encryption_options schema = %v", props["encryption_options"])
	}
	sub := opts["properties"].(map[string]interface{})
	want := map[string]string{"generate_key": "boolean", "passphrase": "string", "key": "string", "pbkdf2iters": "integer"}
	if len(sub) != len(want) {
		t.Errorf("encryption_options properties = %v, want exactly %v", schemaKeysOf(sub), want)
	}
	for k, typ := range want {
		p, _ := sub[k].(map[string]interface{})
		if p["type"] != typ {
			t.Errorf("encryption_options.%s type = %v, want %s", k, p["type"], typ)
		}
	}
}

func schemaKeysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func encOptsError(opts map[string]interface{}) error {
	_, _, err := parseDatasetEncryption(map[string]interface{}{"encryption": true, "encryption_options": opts})
	return err
}

func TestEncryptionOptionBoundaries(t *testing.T) {
	pass := func(n int) string { return strings.Repeat("p", n) }
	tests := []struct {
		name string
		opts map[string]interface{}
		ok   bool
	}{
		{"pbkdf2iters 99999", map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(99999)}, false},
		{"pbkdf2iters 100000", map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(100000)}, true},
		{"pbkdf2iters 100001", map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(100001)}, true},
		{"pbkdf2iters MaxInt32", map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(math.MaxInt32)}, true},
		{"pbkdf2iters MaxInt32+1", map[string]interface{}{"passphrase": testPassphrase, "pbkdf2iters": float64(math.MaxInt32) + 1}, false},
		{"key 63", map[string]interface{}{"key": strings.Repeat("a", 63)}, false},
		{"key 64", map[string]interface{}{"key": strings.Repeat("a", 64)}, true},
		{"key 64 upper", map[string]interface{}{"key": strings.Repeat("A", 64)}, true},
		{"key 65", map[string]interface{}{"key": strings.Repeat("a", 65)}, false},
		{"passphrase 7", map[string]interface{}{"passphrase": pass(7)}, false},
		{"passphrase 8", map[string]interface{}{"passphrase": pass(8)}, true},
		{"passphrase 512 bytes", map[string]interface{}{"passphrase": pass(512)}, true},
		{"passphrase 513 bytes", map[string]interface{}{"passphrase": pass(513)}, false},
		{"passphrase 170 three-byte runes (510 bytes)", map[string]interface{}{"passphrase": strings.Repeat("€", 170)}, true},
		{"passphrase 171 three-byte runes (513 bytes)", map[string]interface{}{"passphrase": strings.Repeat("€", 171)}, false},
		{"passphrase 7 multibyte runes", map[string]interface{}{"passphrase": strings.Repeat("€", 7)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := encOptsError(tt.opts)
			if tt.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("expected an error")
			}
			if err != nil && strings.Contains(err.Error(), strings.Repeat("p", 8)) {
				t.Errorf("error leaks the passphrase: %v", err)
			}
		})
	}
	if err := encOptsError(map[string]interface{}{"passphrase": pass(513)}); err == nil || !strings.Contains(err.Error(), "at most 512 bytes") {
		t.Errorf("long passphrase error = %v", err)
	}
}

func TestValidateEncryptionOptionsDeterministicErrors(t *testing.T) {
	opts := map[string]interface{}{"zeta": 1, "alpha": 1, "mid": 1, "algorithm": "AES-256-GCM"}
	first := validateEncryptionOptions(opts)
	if first == nil || !strings.Contains(first.Error(), "algorithm") {
		t.Fatalf("first error = %v, want the algorithm error (sorted first)", first)
	}
	for i := 0; i < 200; i++ {
		if err := validateEncryptionOptions(opts); err == nil || err.Error() != first.Error() {
			t.Fatalf("run %d: error = %v, want %v", i, err, first)
		}
	}
	unknown := map[string]interface{}{"zeta": 1, "alpha": 1, "generate_key": true}
	want := validateEncryptionOptions(unknown)
	if want == nil || !strings.Contains(want.Error(), `"alpha"`) {
		t.Fatalf("error = %v, want alpha reported first", want)
	}
	for i := 0; i < 200; i++ {
		if err := validateEncryptionOptions(unknown); err.Error() != want.Error() {
			t.Fatalf("run %d: error = %v, want %v", i, err, want)
		}
	}
}

func TestKeyBackupGuidancePerMode(t *testing.T) {
	for _, tt := range []struct {
		opts map[string]interface{}
		want []string
	}{
		{map[string]interface{}{"generate_key": true}, []string{"key database", "not returned", "Export Key", "TrueNAS web UI"}},
		{map[string]interface{}{"passphrase": testPassphrase}, []string{"does not store the passphrase", "data on this dataset is lost"}},
		{map[string]interface{}{"key": testKeyHex}, []string{"must keep a copy", "does not return"}},
	} {
		client, _ := newFakeClientWith(t, encFakeResults(), nil)
		out, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/secure", "encryption": true, "encryption_options": tt.opts})
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		w, _ := resp["encryption_warning"].(string)
		for _, s := range tt.want {
			if !strings.Contains(w, s) {
				t.Errorf("warning %q missing %q", w, s)
			}
		}
		if strings.Contains(w, "Storage > Pools") || strings.Contains(out, testPassphrase) || strings.Contains(out, testKeyHex) {
			t.Errorf("stale path or secret in response: %s", out)
		}
	}
	// A plain dataset gets no warning.
	client, _ := newFakeClientWith(t, map[string]interface{}{"pool.dataset.create": map[string]interface{}{"id": "tank/p", "name": "tank/p", "encrypted": false, "key_loaded": true}}, nil)
	out, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/p"})
	if err != nil || strings.Contains(out, "encryption_warning") {
		t.Errorf("unexpected warning: %s %v", out, err)
	}
	// An encrypted child that inherits gets the generic note.
	client, _ = newFakeClientWith(t, map[string]interface{}{"pool.dataset.create": map[string]interface{}{"id": "tank/e/c", "name": "tank/e/c", "encrypted": true}}, nil)
	out, err = handleCreateDataset(client, map[string]interface{}{"name": "tank/e/c"})
	if err != nil || !strings.Contains(out, "This dataset is encrypted") {
		t.Errorf("generic note missing: %s %v", out, err)
	}
}

func TestRedactLiteralSecrets(t *testing.T) {
	tricky := `pa"ss\wörd-123`
	opts := map[string]interface{}{"passphrase": tricky}
	jsonEsc, _ := json.Marshal(tricky)
	msg := "bad value " + tricky + " and " + string(jsonEsc[1:len(jsonEsc)-1]) + " done"
	got := redactLiteralSecrets(msg, opts)
	if strings.Contains(got, tricky) || strings.Contains(got, string(jsonEsc[1:len(jsonEsc)-1])) || strings.Count(got, "[REDACTED]") != 2 {
		t.Errorf("got %q", got)
	}
	keyOpts := map[string]interface{}{"key": testKeyHex}
	got = redactLiteralSecrets("invalid "+strings.ToUpper(testKeyHex)+" and "+testKeyHex, keyOpts)
	if strings.Contains(strings.ToLower(got), testKeyHex) {
		t.Errorf("key survived: %q", got)
	}
	if got := redactLiteralSecrets("nothing here", nil); got != "nothing here" {
		t.Errorf("got %q", got)
	}
	if got := redactLiteralSecrets("a.b", map[string]interface{}{"passphrase": "a.b"}); got != "[REDACTED]" {
		t.Errorf("regex metacharacters not quoted: %q", got)
	}
}

// A middleware error that quotes the supplied values in plain prose (without
// a field label the redactor could key on) must not leak them.
func TestCreateDatasetMiddlewareErrorScrubsLiteralSecrets(t *testing.T) {
	for name, opts := range map[string]map[string]interface{}{
		"passphrase": {"passphrase": testPassphrase},
		"key":        {"key": testKeyHex},
	} {
		t.Run(name, func(t *testing.T) {
			secret := testPassphrase
			if name == "key" {
				secret = testKeyHex
			}
			fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method == "pool.dataset.create" {
					return nil, map[string]interface{}{"code": -32001, "message": "[EINVAL] rejected value '" + secret + "' for new dataset"}, true
				}
				return nil, nil, false
			}}
			client, _ := newFakeClientFrom(t, fake)
			_, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/secure", "encryption": true, "encryption_options": opts})
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestCreateDatasetEncryptionGuidanceText(t *testing.T) {
	r := NewRegistry(nil, nil)
	desc := r.tools["create_dataset"].Definition.Description
	for _, want := range []string{"do not default to one", "key database", "Export Key", "TrueNAS does not store it", "must keep it",
		"passphrase-encrypted parent", "not configurable through this tool", "AES-256-GCM"} {
		if !strings.Contains(desc, want) {
			t.Errorf("create_dataset description missing %q", want)
		}
	}
	for _, bad := range []string{"Storage > Pools", "simplest", "recommended for simplicity"} {
		if strings.Contains(desc, bad) {
			t.Errorf("create_dataset description still contains %q", bad)
		}
	}
}

// TestKeyBackupGuidanceDoesNotNameExportTool: the export_dataset_key tool is
// not part of this server, so guidance must point at the TrueNAS web UI only.
func TestKeyBackupGuidanceDoesNotNameExportTool(t *testing.T) {
	for _, mode := range []string{"generate_key", "passphrase", "key", ""} {
		if g := keyBackupGuidance(mode); strings.Contains(g, "export_dataset_key") {
			t.Errorf("guidance for %q names export_dataset_key: %q", mode, g)
		}
	}
	if g := keyBackupGuidance("generate_key"); !strings.Contains(g, "Export Key") || !strings.Contains(g, "web UI") {
		t.Errorf("generate_key guidance does not point at the web UI: %q", g)
	}
	tool := NewRegistry(nil, nil).tools["create_dataset"]
	blob, err := json.Marshal(tool.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "export_dataset_key") {
		t.Error("create_dataset definition names export_dataset_key")
	}
}

// createWithEchoingMiddleware runs create_dataset against a middleware that
// fails with msg, and returns the resulting error text.
func createWithEchoingMiddleware(t *testing.T, name, msg string, opts map[string]interface{}) string {
	t.Helper()
	fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.create" {
			return nil, map[string]interface{}{"code": -32001, "message": msg}, true
		}
		return nil, nil, false
	}}
	client, _ := newFakeClientFrom(t, fake)
	_, err := handleCreateDataset(client, map[string]interface{}{"name": name, "encryption": true, "encryption_options": opts})
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

// paramsBlock returns the request block of an API error text.
func paramsBlock(t *testing.T, text string) string {
	t.Helper()
	i := strings.Index(text, "\n  Params: ")
	if i < 0 {
		t.Fatalf("no params block in %q", text)
	}
	block := text[i:]
	if j := strings.Index(block, "\n\nDetails: "); j >= 0 {
		block = block[:j]
	}
	if j := strings.Index(block, "\n\nTrace: "); j >= 0 {
		block = block[:j]
	}
	return block
}

// TestCreateDatasetErrorScrubLeavesRequestBlockAlone: the literal-secret scrub
// covers what the server said, not the params block, which is already masked
// by key name. A passphrase that happens to equal ordinary text must not
// garble that block.
func TestCreateDatasetErrorScrubLeavesRequestBlockAlone(t *testing.T) {
	t.Run("passphrase equal to a field name", func(t *testing.T) {
		pass := "encryption" // appears in the keys encryption and encryption_options
		text := createWithEchoingMiddleware(t, "tank/secure", "[EINVAL] bad value '"+pass+"'", map[string]interface{}{"passphrase": pass})
		block := paramsBlock(t, text)
		for _, want := range []string{`"encryption": true`, `"encryption_options": {`, `"passphrase": "[REDACTED]"`} {
			if !strings.Contains(block, want) {
				t.Errorf("params block lost %q:\n%s", want, text)
			}
		}
		if strings.Contains(block, "[REDACTED]_options") {
			t.Errorf("params key garbled:\n%s", text)
		}
		if strings.Contains(text, "bad value '"+pass+"'") || !strings.Contains(text, "bad value '[REDACTED]'") {
			t.Errorf("server message not scrubbed:\n%s", text)
		}
	})
	t.Run("passphrase equal to the dataset name", func(t *testing.T) {
		pass := "tank/secure-data"
		text := createWithEchoingMiddleware(t, pass, "[EINVAL] dataset "+pass+" rejected", map[string]interface{}{"passphrase": pass})
		if block := paramsBlock(t, text); !strings.Contains(block, `"name": "`+pass+`"`) {
			t.Errorf("dataset name masked in params block:\n%s", text)
		}
		if strings.Contains(text, "dataset "+pass) {
			t.Errorf("server message not scrubbed:\n%s", text)
		}
	})
	t.Run("whitespace-only passphrase", func(t *testing.T) {
		pass := strings.Repeat(" ", 8)
		text := createWithEchoingMiddleware(t, "tank/secure", "[EINVAL] bad value '"+pass+"'", map[string]interface{}{"passphrase": pass})
		want := "\n  Params: [\n    {\n      \"create_ancestors\": true,\n      \"encryption\": true,\n      \"encryption_options\": {\n        \"passphrase\": \"[REDACTED]\"\n      },\n      \"inherit_encryption\": false,\n      \"name\": \"tank/secure\",\n      \"type\": \"FILESYSTEM\"\n    }\n  ]"
		if block := paramsBlock(t, text); block != want {
			t.Errorf("params block changed:\n got %q\nwant %q", block, want)
		}
		if strings.Contains(text, "bad value '"+pass+"'") {
			t.Errorf("server message not scrubbed:\n%q", text)
		}
	})
	t.Run("upper-case passphrase echoed by the server", func(t *testing.T) {
		pass := testPassphrase
		text := createWithEchoingMiddleware(t, "tank/secure", "[EINVAL] bad value '"+strings.ToUpper(pass)+"'", map[string]interface{}{"passphrase": pass})
		if strings.Contains(strings.ToLower(text), pass) {
			t.Errorf("passphrase survived in some case:\n%s", text)
		}
		if !strings.Contains(text, "bad value '[REDACTED]'") {
			t.Errorf("server message not scrubbed:\n%s", text)
		}
	})
}

func TestRedactLiteralSecretsPassphraseIsCaseInsensitive(t *testing.T) {
	opts := map[string]interface{}{"passphrase": "Correct-Horse-Battery"}
	got := redactLiteralSecrets("a CORRECT-HORSE-BATTERY b correct-horse-battery c Correct-Horse-Battery", opts)
	if strings.Count(got, "[REDACTED]") != 3 || strings.Contains(strings.ToLower(got), "horse") {
		t.Errorf("got %q", got)
	}
}

func TestCreateDatasetErrorScrubCoversDetailsAndTrace(t *testing.T) {
	fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.create" {
			return nil, map[string]interface{}{"code": -32001, "message": "failed", "data": map[string]interface{}{
				"error": 22, "errname": "EINVAL", "reason": "bad " + testPassphrase,
				"trace": "Traceback: value " + testPassphrase,
				"extra": []interface{}{[]interface{}{"pool_dataset_create.encryption_options", "echoed " + testPassphrase, 22}},
			}}, true
		}
		return nil, nil, false
	}}
	client, _ := newFakeClientFrom(t, fake)
	_, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/secure", "encryption": true, "encryption_options": map[string]interface{}{"passphrase": testPassphrase}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testPassphrase) {
		t.Errorf("passphrase leaked:\n%s", err)
	}
	for _, want := range []string{"bad [REDACTED]", "Traceback: value [REDACTED]", "echoed [REDACTED]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q:\n%s", want, err)
		}
	}
}
