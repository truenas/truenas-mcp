package truenas

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authWithTLSConfig runs connect+authenticate against srv using cfg.
func authWithTLSConfig(t *testing.T, srv *httptest.Server, cfg *tls.Config) error {
	t.Helper()
	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + apiPath
	client, err := NewClientWithOptions(endpoint, testAPIKey, cfg, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	defer client.Close()
	return client.Authenticate()
}

// serverCertPEM writes srv's self-signed certificate to a temp PEM file.
func serverCertPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}
	return path
}

func TestTLSDefaultRejectsUntrustedCert(t *testing.T) {
	srv := startFakeTrueNAS(t, false)

	cfg, err := NewTLSConfig(false, "")
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	authErr := authWithTLSConfig(t, srv, cfg)
	if authErr == nil {
		t.Fatal("connection to a server with an untrusted certificate succeeded; verification is not happening")
	}
	if !strings.Contains(authErr.Error(), "--tls-ca") {
		t.Errorf("expected certificate-failure hint in error, got: %v", authErr)
	}
}

func TestTLSInsecureOptInAccepts(t *testing.T) {
	srv := startFakeTrueNAS(t, false)

	cfg, err := NewTLSConfig(true, "")
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	if authErr := authWithTLSConfig(t, srv, cfg); authErr != nil {
		t.Fatalf("insecure mode should accept any certificate, got: %v", authErr)
	}
}

func TestTLSCAPinnedCertAccepts(t *testing.T) {
	srv := startFakeTrueNAS(t, false)

	cfg, err := NewTLSConfig(false, serverCertPEM(t, srv))
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	if authErr := authWithTLSConfig(t, srv, cfg); authErr != nil {
		t.Fatalf("pinned server certificate should be trusted, got: %v", authErr)
	}
}

func TestTLSCAPinDoesNotDisableVerification(t *testing.T) {
	// A pinned CA must only add trust for that CA - a server presenting a
	// different untrusted certificate must still be rejected.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "unrelated.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}

	cfg, err := NewTLSConfig(false, path)
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	srv := startFakeTrueNAS(t, false)
	if authErr := authWithTLSConfig(t, srv, cfg); authErr == nil {
		t.Fatal("server with a certificate outside the pinned CA was accepted")
	}
}

func TestNewTLSConfigErrors(t *testing.T) {
	if _, err := NewTLSConfig(false, "/nonexistent/cert.pem"); err == nil {
		t.Error("expected error for missing CA file")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	os.WriteFile(junk, []byte("not a certificate"), 0o600)
	if _, err := NewTLSConfig(false, junk); err == nil {
		t.Error("expected error for PEM file without certificates")
	}
}

// A certificate without IP SANs, reached by IP, gets the name-mismatch hint,
// not the --tls-ca one.
func TestTLSHintForIPWithoutIPSANs(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "truenas.example"},
		DNSNames:              []string{"truenas.example"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	err = authWithTLSConfig(t, srv, &tls.Config{RootCAs: pool}) // srv.URL is https://127.0.0.1:port
	if err == nil {
		t.Fatal("certificate without IP SANs accepted for an IP address")
	}
	msg := err.Error()
	for _, want := range []string{
		"doesn't contain any IP SANs",
		"connect using a host name listed in the certificate",
		"with IP 127.0.0.1 in its subject alternative names",
		"--tls-ca cannot fix a name mismatch",
		"--insecure",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "--tls-ca <cert.pem>") {
		t.Errorf("name mismatch got the unknown-authority hint: %v", msg)
	}
}

func TestTLSHint(t *testing.T) {
	for _, tc := range []struct {
		err  error
		host string
		want string
	}{
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "192.0.2.10"}, "192.0.2.10", "with IP 192.0.2.10 in its subject alternative names"},
		{x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"a.example"}}, Host: "b.example"}, "b.example", "subject alternative names include b.example"},
		{fmt.Errorf("tls: %w", x509.UnknownAuthorityError{}), "192.0.2.10", "--tls-ca <cert.pem>"},
		{errors.New("dial tcp 192.0.2.10:443: connection refused"), "192.0.2.10", ""},
	} {
		got := tlsHint(tc.err, tc.host)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("tlsHint(%v): got %q, want %q", tc.err, got, tc.want)
		}
	}
}
