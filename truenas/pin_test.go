package truenas

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParsePin(t *testing.T) {
	fp := "SHA256:" + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	for _, in := range []string{
		fp,
		strings.ToLower(fp),
		strings.Repeat("ab", 32),
		"sha256:" + strings.Repeat("Ab", 32),
		"  " + fp + "\n",
	} {
		got, err := ParsePin(in)
		if err != nil || got != fp {
			t.Errorf("ParsePin(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "SHA256:", "SHA256:abcd", strings.Repeat("zz", 32), "SHA1:" + strings.Repeat("ab", 32)} {
		if _, err := ParsePin(bad); err == nil {
			t.Errorf("ParsePin(%q) accepted", bad)
		}
	}
}

// TRUENAS_TLS_PIN at runtime: the pinned certificate is accepted even under
// a host name it does not list, and any other certificate is refused.
func TestPinnedTLSConfigAtRuntime(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	endpoint := f.wsURL("localhost") // the test certificate lists 127.0.0.1 and example.com only

	cfg, err := NewPinnedTLSConfig(Fingerprint(f.Certificate().Raw))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClientWithOptions(endpoint, testAPIKey, cfg, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatalf("pinned certificate refused: %v", err)
	}

	wrong, _ := NewPinnedTLSConfig("SHA256:" + strings.Repeat("00", 32))
	c2, _ := NewClientWithOptions(endpoint, testAPIKey, wrong, Options{APIUser: testAPIUser})
	defer c2.Close()
	err = c2.Authenticate(context.Background())
	var pe *PinMismatchError
	if !errors.As(err, &pe) {
		t.Fatalf("wrong pin: got %v, want a PinMismatchError", err)
	}
	if pe.Got != Fingerprint(f.Certificate().Raw) || !strings.Contains(err.Error(), "update the pin") {
		t.Errorf("error %v", err)
	}
	if f.logins(t) != 1 {
		t.Errorf("%d logins; the wrong-pin connection must not send one", f.logins(t))
	}
}

func TestCapturingTLSConfig(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	ca := serverCertPEM(t, f.Server)

	dial := func(host, caPath string) error {
		cfg, err := NewCapturingTLSConfig(caPath, host)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Dial(context.Background(), f.wsURL(host), cfg, "none")
		if err == nil {
			s.Close()
		}
		return err
	}
	err := dial("127.0.0.1", "")
	var ue *UntrustedCertError
	if !errors.As(err, &ue) || ue.Fingerprint() != Fingerprint(f.Certificate().Raw) || !IsTLSError(err) {
		t.Fatalf("system roots: got %v, want an UntrustedCertError with the fingerprint", err)
	}
	if err := dial("127.0.0.1", ca); err != nil {
		t.Errorf("trusted CA refused: %v", err)
	}
	err = dial("localhost", ca)
	if !errors.As(err, &ue) {
		t.Fatalf("name mismatch: got %v, want an UntrustedCertError", err)
	}
	if !strings.Contains(err.Error(), "connect using a host name listed in the certificate") {
		t.Errorf("name mismatch lost its hint: %v", err)
	}
}

func TestSessionScrubsAddedSecrets(t *testing.T) {
	const secret = "SESSION-SECRET-MARKER"
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.errors["system.info"] = map[string]interface{}{
			"code": -32001, "message": "Method call error",
			"data": map[string]interface{}{"error": 22, "errname": "EINVAL", "reason": "bad value " + secret},
		}
	})
	cfg, _ := NewTLSConfig(true, "")
	s, err := Dial(context.Background(), f.wsURL("127.0.0.1"), cfg, "none")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddSecret(secret)
	if _, err := s.Call(context.Background(), "auth.login_ex", map[string]interface{}{"mechanism": "API_KEY_PLAIN"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Call(context.Background(), "system.info")
	var ae *APICallError
	if !errors.As(err, &ae) || ae.Code != 22 {
		t.Fatalf("got %v, want an APICallError with errno 22", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("secret not scrubbed: %v", err)
	}
}
