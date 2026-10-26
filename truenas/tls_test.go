package truenas

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
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
