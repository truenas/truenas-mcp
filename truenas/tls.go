package truenas

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// NewTLSConfig returns the TLS configuration for connecting to TrueNAS.
//
// By default the server certificate is fully verified (chain and hostname)
// against the system trust store. caPath, if non-empty, names a PEM file
// whose certificates are trusted instead - typically the TrueNAS server's
// own self-signed certificate, or a private CA. insecure disables
// verification entirely and must remain an explicit opt-in: it leaves the
// connection open to man-in-the-middle interception.
func NewTLSConfig(insecure bool, caPath string) (*tls.Config, error) {
	if insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	if caPath == "" {
		return &tls.Config{}, nil
	}
	pool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool}, nil
}

func loadCAPool(caPath string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read TLS CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates found in %s", caPath)
	}
	return pool, nil
}

// Fingerprint returns the SHA-256 fingerprint of a DER certificate in the
// form SHA256:AB:CD:..., the same hex digits openssl x509 -fingerprint
// -sha256 prints.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return formatFingerprint(sum[:])
}

func formatFingerprint(sum []byte) string {
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return "SHA256:" + strings.Join(parts, ":")
}

// ParsePin reads a SHA-256 certificate fingerprint: 64 hex digits, with or
// without colons, in either case, optionally prefixed with "SHA256:" (also
// in either case). It returns the canonical SHA256:AB:CD:... form.
func ParsePin(s string) (string, error) {
	sum, err := parsePin(s)
	if err != nil {
		return "", err
	}
	return formatFingerprint(sum), nil
}

func parsePin(s string) ([]byte, error) {
	v := strings.TrimSpace(s)
	if len(v) >= 7 && strings.EqualFold(v[:7], "sha256:") {
		v = v[7:]
	}
	v = strings.ReplaceAll(v, ":", "")
	sum, err := hex.DecodeString(v)
	if err != nil || len(sum) != sha256.Size {
		return nil, errors.New("not a SHA-256 certificate fingerprint (want SHA256: followed by 64 hex digits, colons optional)")
	}
	return sum, nil
}

// PinMismatchError is returned when the server's leaf certificate does not
// match the configured SHA-256 pin.
type PinMismatchError struct {
	Got string // fingerprint of the certificate the server presented
}

func (e *PinMismatchError) Error() string {
	return "the TrueNAS certificate does not match the pinned fingerprint: the server presented " + e.Got
}

// NewPinnedTLSConfig trusts exactly the leaf certificate whose SHA-256
// fingerprint is pin (see ParsePin). The chain and host name are not
// checked: the pin replaces them, so a self-signed certificate, or one
// reached by an IP address it does not list, is accepted as long as it is
// the pinned certificate. Any other certificate is refused with a
// *PinMismatchError.
func NewPinnedTLSConfig(pin string) (*tls.Config, error) {
	want, err := parsePin(pin)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the TrueNAS server presented no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return &PinMismatchError{Got: formatFingerprint(got[:])}
			}
			return nil
		},
	}, nil
}

// UntrustedCertError is returned when standard verification (chain and
// host name) of the server certificate fails. It carries the leaf
// certificate so the caller can show its fingerprint and offer to pin it.
type UntrustedCertError struct {
	Leaf *x509.Certificate
	Err  error
}

func (e *UntrustedCertError) Error() string {
	return "the TrueNAS certificate is not trusted (" + Fingerprint(e.Leaf.Raw) + "): " + e.Err.Error()
}

func (e *UntrustedCertError) Unwrap() error { return e.Err }

// Fingerprint returns the SHA-256 fingerprint of the untrusted leaf.
func (e *UntrustedCertError) Fingerprint() string { return Fingerprint(e.Leaf.Raw) }

// NewCapturingTLSConfig verifies the server certificate exactly as the
// default configuration does (chain against caPath or the system roots,
// and serverName), but on failure returns an *UntrustedCertError that
// carries the leaf certificate. serverName is the host being dialled.
func NewCapturingTLSConfig(caPath, serverName string) (*tls.Config, error) {
	var roots *x509.CertPool
	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		roots = pool
	}
	return &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, // verified in VerifyConnection, as crypto/tls would
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the TrueNAS server presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				DNSName:       serverName,
				Roots:         roots,
				Intermediates: inter,
			})
			if err != nil {
				return &UntrustedCertError{Leaf: cs.PeerCertificates[0], Err: err}
			}
			return nil
		},
	}, nil
}

// IsTLSError reports whether err is a failure to establish trust in the
// server certificate (untrusted, wrong pin, or a TLS verification error).
func IsTLSError(err error) bool {
	var ue *UntrustedCertError
	var pe *PinMismatchError
	var ve *tls.CertificateVerificationError
	var he x509.HostnameError
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	return errors.As(err, &ue) || errors.As(err, &pe) || errors.As(err, &ve) ||
		errors.As(err, &he) || errors.As(err, &ua) || errors.As(err, &ci)
}
