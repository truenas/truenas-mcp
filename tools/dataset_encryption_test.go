package tools

import (
	"encoding/json"
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
		{"algorithm", map[string]interface{}{"encryption": true, "encryption_options": map[string]interface{}{"generate_key": true, "algorithm": "AES-256-GCM"}}, "algorithm is not supported"},
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
		t.Errorf("encryption_options properties = %v, want exactly %v", keysOf(sub), want)
	}
	for k, typ := range want {
		p, _ := sub[k].(map[string]interface{})
		if p["type"] != typ {
			t.Errorf("encryption_options.%s type = %v, want %s", k, p["type"], typ)
		}
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
