package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/truenas"
)

func TestInsecureWarningPrefersTLSCA(t *testing.T) {
	msg := insecureWarning(false)
	for _, want := range []string{"WARNING", "man-in-the-middle", "Prefer --tls-ca <cert.pem>", "TRUENAS_TLS_CA", "keep verification on"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "not used") {
		t.Errorf("warning mentions an ignored CA file when none was given: %q", msg)
	}
	if !strings.Contains(insecureWarning(true), "CA file you gave is not used") {
		t.Error("warning does not say that --insecure overrides --tls-ca")
	}
}

func TestStartupErrorDistinguishesConnectFromAuth(t *testing.T) {
	dial := fmt.Errorf("wrapped: %w", &truenas.ConnectError{Err: errors.New("dial tcp 192.0.2.1:443: i/o timeout")})
	got := startupAuthError(dial)
	if !strings.HasPrefix(got, "Could not connect to TrueNAS") || strings.Contains(strings.ToLower(got), "authenticate") ||
		!strings.Contains(got, "i/o timeout") || strings.Contains(got, "connection failed") {
		t.Errorf("connect failure worded as %q", got)
	}

	// A reconnect after a connection dropped mid-login: a login was attempted.
	re := startupAuthError(&truenas.ConnectError{Err: errors.New("dial tcp 192.0.2.1:443: i/o timeout"), LoginAttempted: true})
	if strings.Contains(re, "no login was attempted") || !strings.Contains(re, "lost during login") || !strings.Contains(re, "i/o timeout") {
		t.Errorf("reconnect failure worded as %q", re)
	}
	if !strings.Contains(got, "no login was attempted") {
		t.Errorf("first connect failure worded as %q", got)
	}

	bad := &truenas.EndpointError{Err: errors.New("bad port")}
	if got := startupAuthError(bad); got != "invalid TrueNAS host or URL: bad port" {
		t.Errorf("endpoint error worded as %q", got)
	}

	auth := startupAuthError(errors.New("authentication failed: response_type=AUTH_ERR"))
	if want := "Failed to authenticate with TrueNAS: authentication failed: response_type=AUTH_ERR"; auth != want {
		t.Errorf("auth failure worded as %q, want %q", auth, want)
	}
}
