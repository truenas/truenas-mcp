package truenas

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const (
	testAPIKey  = "1-verysecretapikey"
	testAPIUser = "svc-mcp"
)

func TestRedactParamsAuthMethods(t *testing.T) {
	methods := []string{"auth.login_ex", "auth.login_with_api_key", "auth.login"}
	for _, method := range methods {
		redacted := redactParams(method, []interface{}{"1-supersecretapikey"})
		out, _ := json.Marshal(redacted)
		if strings.Contains(string(out), "supersecretapikey") {
			t.Errorf("%s: API key leaked into log output: %s", method, out)
		}
		if redacted[0] != "[REDACTED]" {
			t.Errorf("%s: expected [REDACTED], got %v", method, redacted[0])
		}
	}
}

func TestRedactParamsSensitiveKeys(t *testing.T) {
	params := []interface{}{
		map[string]interface{}{
			"domainname": "example.com",
			"bindpw":     "hunter2",
			"configuration": map[string]interface{}{
				"password": "hunter3",
				"kerberos_realm": map[string]interface{}{
					"admin_password": "hunter4",
				},
			},
		},
	}
	redacted := redactParams("directoryservices.update", params)
	out, _ := json.Marshal(redacted)
	for _, secret := range []string{"hunter2", "hunter3", "hunter4"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("secret %q leaked into log output: %s", secret, out)
		}
	}
	if !strings.Contains(string(out), "example.com") {
		t.Errorf("non-sensitive value was redacted: %s", out)
	}
}

func TestRedactParamsLeavesOriginalUntouched(t *testing.T) {
	payload := map[string]interface{}{"bindpw": "hunter2"}
	redactParams("directoryservices.update", []interface{}{payload})
	if payload["bindpw"] != "hunter2" {
		t.Error("redactParams mutated the original params, which would corrupt the request sent on the wire")
	}
}

func TestRedactParamsNonSensitivePassthrough(t *testing.T) {
	params := []interface{}{"tank", map[string]interface{}{"name": "tank/data"}}
	redacted := redactParams("pool.dataset.create", params)
	out, _ := json.Marshal(redacted)
	orig, _ := json.Marshal(params)
	if string(out) != string(orig) {
		t.Errorf("non-sensitive params changed: got %s, want %s", out, orig)
	}
}

func TestRedactJSONForLog(t *testing.T) {
	in := []byte(`{"result":[{"method":"directoryservices.update","arguments":[{"bindpw":"hunter2","domain":"example.com"}],"password":"hunter3"}]}`)
	out := RedactJSONForLog(in)
	if strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "hunter3") {
		t.Errorf("secret survived redaction: %s", out)
	}
	if !strings.Contains(string(out), "example.com") {
		t.Errorf("non-sensitive value was redacted: %s", out)
	}

	// Non-JSON input must pass through unchanged rather than be dropped
	raw := []byte("not json")
	if string(RedactJSONForLog(raw)) != "not json" {
		t.Error("non-JSON input was altered")
	}
}

// runClientSession authenticates, makes one call, and returns everything the
// client logged plus the auth error (if any).
func runClientSession(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + apiPath
	client, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	defer client.Close()

	authErr := client.Authenticate()
	if authErr == nil {
		if _, err := client.Call("test.echo"); err != nil {
			t.Fatalf("Call: %v", err)
		}
	}
	return logBuf.String(), authErr
}

func TestLoggingDefaultModeOmitsBodies(t *testing.T) {
	SetDebugLogging(false)

	logs, authErr := runClientSession(t, startFakeTrueNAS(t, false))
	if authErr != nil {
		t.Fatalf("Authenticate: %v", authErr)
	}

	if strings.Contains(logs, testAPIKey) {
		t.Errorf("API key leaked into log output:\n%s", logs)
	}
	if strings.Contains(logs, "responsesecret9") {
		t.Errorf("response secret leaked into log output:\n%s", logs)
	}
	if strings.Contains(logs, "params") {
		t.Errorf("request body logged outside debug mode:\n%s", logs)
	}
	if !strings.Contains(logs, "method=auth.login_ex") {
		t.Errorf("expected request metadata line in log output:\n%s", logs)
	}
	if !strings.Contains(logs, "result=") {
		t.Errorf("expected response metadata line in log output:\n%s", logs)
	}
}

func TestLoggingDebugModeRedactsSecrets(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)

	logs, authErr := runClientSession(t, startFakeTrueNAS(t, false))
	if authErr != nil {
		t.Fatalf("Authenticate: %v", authErr)
	}

	if strings.Contains(logs, testAPIKey) {
		t.Errorf("API key leaked into debug log output:\n%s", logs)
	}
	if strings.Contains(logs, "responsesecret9") {
		t.Errorf("response secret leaked into debug log output:\n%s", logs)
	}
	if !strings.Contains(logs, "[REDACTED]") {
		t.Errorf("expected redacted request body in debug log output:\n%s", logs)
	}
	if !strings.Contains(logs, "example.com") {
		t.Errorf("expected non-sensitive response content in debug log output:\n%s", logs)
	}
}

func TestServerEchoedKeyIsScrubbed(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)

	logs, authErr := runClientSession(t, startFakeTrueNAS(t, true))
	if authErr == nil {
		t.Fatal("expected authentication to fail")
	}

	if strings.Contains(logs, testAPIKey) {
		t.Errorf("server-echoed API key leaked into log output:\n%s", logs)
	}
	if strings.Contains(authErr.Error(), testAPIKey) {
		t.Errorf("server-echoed API key leaked into error message: %s", authErr)
	}
	if !strings.Contains(authErr.Error(), "[REDACTED]") {
		t.Errorf("expected scrubbed error message, got: %s", authErr)
	}
}

func TestRedactionSharedWithLogsAndErrors(t *testing.T) {
	pem := "-----BEGIN EC PRIVATE KEY-----\\nFAKEFAKEnotreal\\n-----END EC PRIVATE KEY-----"
	out := string(RedactJSONForLog([]byte(`{"result":[{"name":"cert","privatekey":"` + pem + `","notes":"` + pem + `"}]}`)))
	if strings.Contains(out, "FAKEFAKEnotreal") || !strings.Contains(out, `"name":"cert"`) {
		t.Errorf("debug log not redacted: %s", out)
	}

	c := &Client{apiKey: testAPIKey}
	msg := c.scrubSecrets(`[EINVAL] {'password': 'fake-user-pw', 'username': 'alice'} key ` + testAPIKey)
	if strings.Contains(msg, "fake-user-pw") || strings.Contains(msg, testAPIKey) || !strings.Contains(msg, "'username': 'alice'") {
		t.Errorf("server-supplied text not redacted: %s", msg)
	}
}
