package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/internal/mwtime"
	"github.com/truenas/truenas-mcp/truenas"
)

// Exit codes of truenas-mcp setup.
const (
	exitSetupOK         = 0
	exitSetupFailure    = 1  // anything not listed below
	exitSetupUsage      = 2  // bad flags or arguments
	exitSetupAuth       = 3  // login refused (wrong password, expired, redirect, mechanism)
	exitSetupOTP        = 4  // two-factor code missing or rejected
	exitSetupTLS        = 5  // certificate not trusted, pin mismatch, insecure not confirmed
	exitSetupPermission = 6  // EACCES, STIG mode, full-admin key not confirmed
	exitSetupIO         = 7  // config directory or key file
	exitSetupConnect    = 8  // could not reach TrueNAS
	exitSetupVerify     = 9  // the new key did not work
	exitSetupVersion    = 10 // TrueNAS older than 25.10
)

const (
	setupPasswordEnv = "TRUENAS_SETUP_PASSWORD"
	setupOTPEnv      = "TRUENAS_SETUP_OTP"
	maxOTPTries      = 3
	setupDialTimeout = 30 * time.Second
	setupCallTimeout = 2 * time.Minute
	maxNameSuffix    = 99
	maxBlindRetries  = 5
	maxKeyNameLen    = 190 // the API allows 200; room for a -NN suffix
	maxExpiry        = 10 * 365 * 24 * time.Hour
)

// setupError carries the exit code and a message that never contains the
// password, OTP or key.
type setupError struct {
	code int
	msg  string
}

func (e *setupError) Error() string { return e.msg }

func failf(code int, format string, args ...interface{}) *setupError {
	return &setupError{code: code, msg: fmt.Sprintf(format, args...)}
}

type setupFlags struct {
	host, user, keyOwner, name, expires string
	rotate, insecure, force             bool
	tlsCA, trustFingerprint, configDir  string
	printConfig, proxy                  string
	passwordStdin, otpStdin             bool
	yesFullAdmin, debug                 bool
}

func newSetupFlagSet(f *setupFlags, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("truenas-mcp setup", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.host, "host", "", "TrueNAS host name, IP or wss:// URL (required)")
	fs.StringVar(&f.user, "user", "", "TrueNAS user to log in as (prompted on a terminal if omitted)")
	fs.StringVar(&f.keyOwner, "key-owner", "", "User that will own the API key (default: the login user); prefer a dedicated user with a limited role")
	fs.StringVar(&f.name, "name", "", "API key name (default truenas-mcp-<this machine's hostname>), cleaned to [a-z0-9.-]")
	fs.StringVar(&f.expires, "expires", "1y", "Key lifetime: a number with h, d, w or y (for example 90d, 1y), or never")
	fs.BoolVar(&f.rotate, "rotate", false, "Reset the existing key with this name (owned by the key owner) instead of creating a new one; replaces the key file")
	fs.StringVar(&f.tlsCA, "tls-ca", "", "PEM certificate or CA to trust for the TrueNAS certificate")
	fs.StringVar(&f.trustFingerprint, "trust-fingerprint", "", "Trust the server certificate with this SHA-256 fingerprint (SHA256:...) without prompting, and pin it")
	fs.BoolVar(&f.insecure, "insecure", false, "Disable certificate verification (UNSAFE; asks you to type a confirmation word)")
	fs.StringVar(&f.configDir, "config-dir", "", "Directory for <host>.key and <host>.pin (default: the user config directory + /truenas-mcp)")
	fs.BoolVar(&f.force, "force", false, "Replace an existing key file")
	fs.StringVar(&f.printConfig, "print-config", "json", "Client snippet to print: "+strings.Join(printConfigFormats, ", "))
	fs.StringVar(&f.proxy, "proxy", "", "Proxy for the TrueNAS connection, as for the server's --proxy")
	fs.BoolVar(&f.passwordStdin, "password-stdin", false, "Read the password as one line from stdin")
	fs.BoolVar(&f.otpStdin, "otp-stdin", false, "Read the two-factor code as one line from stdin (after the password line with --password-stdin)")
	fs.BoolVar(&f.yesFullAdmin, "yes-full-admin", false, "Allow a key whose owner has FULL_ADMIN without the typed confirmation")
	fs.BoolVar(&f.debug, "debug", false, "Log the TrueNAS requests and responses to stderr (credentials redacted)")
	fs.Usage = func() {
		fprint(out, `Usage: truenas-mcp setup --host HOST [flags]

Logs in to TrueNAS once with a user name and password, creates an API key
for the MCP server, saves it to a private file and prints the client
configuration. The password is read from a hidden prompt, from
TRUENAS_SETUP_PASSWORD (unset once read) or with --password-stdin; there is
no password flag. Two-factor codes work the same way (TRUENAS_SETUP_OTP,
--otp-stdin).

Flags:
`)
		fs.PrintDefaults()
	}
	return fs
}

// setup holds the state of one run.
type setup struct {
	io *setupIO
	f  setupFlags

	envPassword, envOTP []byte
	stdinUsed           bool

	endpointHost string // host as given, for the client snippet
	keyPath      string
	pinPath      string
	expiresAt    *time.Time

	tlsConfig *tls.Config
	pin       string // canonical fingerprint when the connection is pinned
	savePin   bool   // the pin was confirmed in this run and should be saved

	sess *truenas.Session
}

// runSetup runs truenas-mcp setup with args (without "setup") and returns
// the process exit code.
func runSetup(args []string, sio *setupIO) int {
	// Taken before anything else, so the variables are gone from the
	// environment even when setup fails on a flag.
	s := &setup{
		io:          sio,
		envPassword: takeEnv(sio, setupPasswordEnv),
		envOTP:      takeEnv(sio, setupOTPEnv),
	}
	defer wipe(s.envPassword)
	defer wipe(s.envOTP)

	prevLog, prevFlags := log.Writer(), log.Flags()
	prevDebug := truenas.DebugLogging()
	defer func() {
		log.SetOutput(prevLog)
		log.SetFlags(prevFlags)
		truenas.SetDebugLogging(prevDebug)
	}()
	log.SetOutput(io.Discard)

	err := s.run(args)
	if s.sess != nil {
		s.sess.Close()
	}
	if err != nil {
		if err.code == exitSetupOK {
			return exitSetupOK
		}
		fprintf(sio.stderr, "truenas-mcp setup: %s\n", err.msg)
		return err.code
	}
	return exitSetupOK
}

func (s *setup) run(args []string) *setupError {
	fset := newSetupFlagSet(&s.f, s.io.stderr)
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &setupError{code: exitSetupOK}
		}
		return failf(exitSetupUsage, "%s", err)
	}
	if fset.NArg() > 0 {
		// Not quoted: a stray argument may be a password typed on the command line.
		return failf(exitSetupUsage, "unexpected positional argument; setup takes flags only (there is no password flag: use the prompt, %s or --password-stdin)", setupPasswordEnv)
	}
	if err := s.checkFlags(); err != nil {
		return err
	}
	if s.f.debug {
		log.SetOutput(s.io.stderr)
		truenas.SetDebugLogging(true)
	}

	u, err := truenas.ParseEndpoint(s.f.host)
	if err != nil {
		return failf(exitSetupUsage, "--host: %s", err)
	}
	s.endpointHost = strings.TrimSpace(s.f.host)
	if err := s.resolvePaths(hostFileStem(u)); err != nil {
		return err
	}
	if !s.f.force && !s.f.rotate {
		if _, err := os.Lstat(s.keyPath); err == nil {
			return failf(exitSetupIO, "%s already exists; pass --force to replace it, or --rotate to reset the key it belongs to", s.keyPath)
		}
	}

	if err := s.connect(u.Hostname()); err != nil {
		return err
	}
	loginName, roles, err2 := s.login()
	if err2 != nil {
		return err2
	}
	version, err2 := s.checkVersion(s.sess)
	if err2 != nil {
		return err2
	}
	owner := s.f.keyOwner
	if owner == "" {
		owner = loginName
	}
	if err := s.checkOwner(owner, loginName, roles); err != nil {
		return err
	}
	key, err2 := s.createKey(owner, loginName)
	if err2 != nil {
		return err2
	}
	if err := s.save(key); err != nil {
		return err
	}

	if _, err := s.callNAS(s.sess, "auth.logout"); err != nil {
		fprintf(s.io.stderr, "Warning: auth.logout failed (%s); the password session ends when the connection closes.\n", shortErr(err))
	}
	s.sess.Close()
	s.sess = nil

	if err := s.verify(owner, key.Key); err != nil {
		return err
	}
	return s.report(owner, key, version)
}

func (s *setup) checkFlags() *setupError {
	f := &s.f
	if strings.TrimSpace(f.host) == "" {
		return failf(exitSetupUsage, "--host is required")
	}
	n := 0
	for _, set := range []bool{f.insecure, f.tlsCA != "", f.trustFingerprint != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return failf(exitSetupUsage, "use only one of --insecure, --tls-ca and --trust-fingerprint")
	}
	if f.trustFingerprint != "" {
		pin, err := truenas.ParsePin(f.trustFingerprint)
		if err != nil {
			return failf(exitSetupUsage, "--trust-fingerprint: %s", err)
		}
		f.trustFingerprint = pin
	}
	if _, err := clientSnippet(f.printConfig, "", nil); err != nil {
		return failf(exitSetupUsage, "%s", err)
	}
	exp, err := parseExpiry(f.expires)
	if err != nil {
		return failf(exitSetupUsage, "--expires: %s", err)
	}
	if exp != 0 {
		t := s.io.now().Add(exp).UTC()
		s.expiresAt = &t
	}
	if f.passwordStdin && len(s.envPassword) > 0 {
		return failf(exitSetupUsage, "both --password-stdin and %s are set; use only one", setupPasswordEnv)
	}
	if f.otpStdin && len(s.envOTP) > 0 {
		return failf(exitSetupUsage, "both --otp-stdin and %s are set; use only one", setupOTPEnv)
	}
	if f.user == "" && (f.passwordStdin || f.otpStdin || !s.io.isTTY) {
		return failf(exitSetupUsage, "--user is required when stdin is not an interactive terminal")
	}
	if f.name == "" {
		h, _ := s.io.hostname()
		f.name = "truenas-mcp-" + h
	}
	f.name = cleanName(f.name)
	if len(f.name) > maxKeyNameLen {
		f.name = strings.TrimRight(f.name[:maxKeyNameLen], "-.")
	}
	if f.name == "" {
		return failf(exitSetupUsage, "--name has no characters left after cleaning to [a-z0-9.-]")
	}
	return nil
}

var expiryRe = regexp.MustCompile(`^(\d{1,5})([hdwy])$`)

// parseExpiry reads --expires. It returns 0 for never.
func parseExpiry(v string) (time.Duration, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "never" {
		return 0, nil
	}
	m := expiryRe.FindStringSubmatch(v)
	if m == nil {
		return 0, errors.New("want a number followed by h, d, w or y (for example 90d or 1y), or never")
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if n == 0 {
		return 0, errors.New("must be longer than zero (or never)")
	}
	if d > maxExpiry {
		return 0, errors.New("must be at most 10 years (or never)")
	}
	return d, nil
}

func (s *setup) resolvePaths(stem string) *setupError {
	dir := s.f.configDir
	if dir == "" {
		base, err := s.io.userConfigDir()
		if err != nil {
			return failf(exitSetupIO, "cannot find the user config directory (%s); pass --config-dir", err)
		}
		dir = filepath.Join(base, "truenas-mcp")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return failf(exitSetupIO, "--config-dir: %s", err)
	}
	s.keyPath = filepath.Join(abs, stem+".key")
	s.pinPath = filepath.Join(abs, stem+".pin")
	return nil
}

// connect opens the password session, settling certificate trust first.
func (s *setup) connect(serverName string) *setupError {
	f := &s.f
	offerPin := false
	switch {
	case f.insecure:
		if err := s.confirmInsecure(); err != nil {
			return err
		}
		cfg, _ := truenas.NewTLSConfig(true, "")
		s.tlsConfig = cfg
	case f.trustFingerprint != "":
		s.usePin(f.trustFingerprint, true)
	case f.tlsCA != "":
		ca, err := filepath.Abs(f.tlsCA)
		if err == nil {
			f.tlsCA = ca
		}
		cfg, err := truenas.NewCapturingTLSConfig(f.tlsCA, serverName)
		if err != nil {
			return failf(exitSetupTLS, "--tls-ca: %s", err)
		}
		s.tlsConfig = cfg
	default:
		if pin, ok := s.savedPin(); ok {
			s.usePin(pin, false)
			fprintf(s.io.stderr, "Using the certificate pin saved in %s.\n", s.pinPath)
			break
		}
		cfg, _ := truenas.NewCapturingTLSConfig("", serverName)
		s.tlsConfig = cfg
		offerPin = true
	}

	sess, err := s.dial()
	var untrusted *truenas.UntrustedCertError
	if err != nil && offerPin && errors.As(err, &untrusted) {
		if err := s.confirmCertificate(untrusted); err != nil {
			return err
		}
		sess, err = s.dial()
	}
	if err != nil {
		return s.connectError(err)
	}
	s.sess = sess
	return nil
}

func (s *setup) usePin(pin string, save bool) {
	cfg, _ := truenas.NewPinnedTLSConfig(pin)
	s.tlsConfig, s.pin, s.savePin = cfg, pin, save
}

// savedPin reads <host>.pin from an earlier run.
func (s *setup) savedPin() (string, bool) {
	b, err := os.ReadFile(s.pinPath)
	if err != nil {
		return "", false
	}
	pin, err := truenas.ParsePin(string(b))
	if err != nil {
		return "", false
	}
	return pin, true
}

func (s *setup) dial() (*truenas.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupDialTimeout)
	defer cancel()
	return truenas.Dial(ctx, s.f.host, s.tlsConfig, s.f.proxy)
}

func (s *setup) connectError(err error) *setupError {
	var ee *truenas.EndpointError
	if errors.As(err, &ee) {
		return failf(exitSetupUsage, "--host: %s", ee)
	}
	var pe *truenas.PinMismatchError
	if errors.As(err, &pe) {
		if s.f.trustFingerprint != "" {
			return failf(exitSetupTLS, "the server presented %s, which does not match --trust-fingerprint %s; refusing to connect", pe.Got, s.f.trustFingerprint)
		}
		return failf(exitSetupTLS, "the server presented %s, which does not match the pin saved in %s; refusing to connect. If the certificate was replaced on purpose, check the new fingerprint, delete that file and run setup again", pe.Got, s.pinPath)
	}
	var ue *truenas.UntrustedCertError
	if errors.As(err, &ue) {
		return failf(exitSetupTLS, "the TrueNAS certificate is not trusted: %s (fingerprint %s)", ue.Err, ue.Fingerprint())
	}
	if truenas.IsTLSError(err) {
		return failf(exitSetupTLS, "%s", err)
	}
	return failf(exitSetupConnect, "could not connect to TrueNAS: %s", err)
}

// confirmInsecure asks for a typed confirmation word before verification
// is turned off. There is no non-interactive way to skip it.
func (s *setup) confirmInsecure() *setupError {
	const word = "insecure"
	if !s.io.isTTY {
		return failf(exitSetupTLS, "--insecure needs a typed confirmation on a terminal; use --trust-fingerprint SHA256:... (or --tls-ca) instead")
	}
	fprintln(s.io.stderr, "WARNING: --insecure turns off certificate verification. Anyone on the network path can impersonate the NAS and capture your password and the new API key.")
	ans, err := s.io.readVisible("Type " + word + " to continue without verification: ")
	if err != nil || strings.TrimSpace(ans) != word {
		return failf(exitSetupTLS, "not confirmed; nothing was sent to TrueNAS")
	}
	return nil
}

// confirmCertificate shows an untrusted certificate and asks the user to
// trust it by typing yes. On yes the connection is pinned to it.
func (s *setup) confirmCertificate(ue *truenas.UntrustedCertError) *setupError {
	fp := ue.Fingerprint()
	w := s.io.stderr
	fprintln(w, "The TrueNAS certificate is not trusted by this computer:")
	fprintf(w, "  reason:      %s\n", ue.Err)
	describeCert(w, ue.Leaf)
	fprintf(w, "  fingerprint: %s\n", fp)
	fprintln(w, "Compare the fingerprint with the certificate the TrueNAS web UI uses (for example: openssl x509 -noout -fingerprint -sha256 -in <certificate file>) before trusting it.")
	if !s.io.isTTY {
		return failf(exitSetupTLS, "stopped at an untrusted certificate; after checking it, run setup again with --trust-fingerprint %s (or --tls-ca)", fp)
	}
	ans, err := s.io.readVisible("Type yes to trust and pin this certificate: ")
	if err != nil || strings.TrimSpace(ans) != "yes" {
		return failf(exitSetupTLS, "certificate not trusted; nothing was sent to TrueNAS")
	}
	s.usePin(fp, true)
	return nil
}

func describeCert(w io.Writer, c *x509.Certificate) {
	fprintf(w, "  subject:     %s\n", printable(c.Subject.String()))
	fprintf(w, "  issuer:      %s\n", printable(c.Issuer.String()))
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) > 0 {
		fprintf(w, "  names:       %s\n", printable(strings.Join(names, ", ")))
	}
	fprintf(w, "  valid:       %s to %s\n", c.NotBefore.UTC().Format(time.DateOnly), c.NotAfter.UTC().Format(time.DateOnly))
}

// printable drops control characters from server-supplied text before it
// reaches the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func (s *setup) callNAS(sess *truenas.Session, method string, params ...interface{}) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupCallTimeout)
	defer cancel()
	return sess.Call(ctx, method, params...)
}

// shortErr is an error's first line: an API error's server message, or a
// transport error. Request parameters (already masked) are left out.
func shortErr(err error) string {
	var ae *truenas.APICallError
	if errors.As(err, &ae) {
		return ae.Message
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

func apiErrno(err error) int {
	var ae *truenas.APICallError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}

const (
	errnoEACCES     = 13
	errnoEINVAL     = 22
	errnoEOPNOTSUPP = 95
)

type loginResponse struct {
	ResponseType string   `json:"response_type"`
	URLs         []string `json:"urls"`
	UserInfo     *struct {
		Username          string   `json:"pw_name"`
		AccountAttributes []string `json:"account_attributes"`
		Privilege         struct {
			Roles []string `json:"roles"`
		} `json:"privilege"`
	} `json:"user_info"`
}

// password returns the login password from the one configured source, and
// whether it came from the terminal (only then is AUTH_ERR re-prompted).
func (s *setup) password() ([]byte, bool, *setupError) {
	switch {
	case s.f.passwordStdin:
		b, err := readLine(s.io.stdin)
		if err != nil {
			return nil, false, failf(exitSetupAuth, "--password-stdin: could not read the password: %s", err)
		}
		s.stdinUsed = true
		return b, false, nil
	case len(s.envPassword) > 0:
		b := append([]byte(nil), s.envPassword...)
		wipe(s.envPassword)
		s.envPassword = nil
		return b, false, nil
	case s.io.isTTY:
		b, err := s.io.readHidden(fmt.Sprintf("Password for %s: ", s.f.user))
		if err != nil {
			return nil, false, failf(exitSetupAuth, "could not read the password: %s", err)
		}
		return b, true, nil
	}
	return nil, false, failf(exitSetupAuth, "no password source: stdin is not a terminal; set %s or pass --password-stdin", setupPasswordEnv)
}

// otp returns the code for attempt try (1-based). The environment value is
// used once; stdin gives one line per attempt; the terminal prompts again.
func (s *setup) otp(try int) ([]byte, *setupError) {
	switch {
	case s.f.otpStdin:
		b, err := readLine(s.io.stdin)
		if err != nil {
			if try > 1 {
				return nil, failf(exitSetupOTP, "the two-factor code was rejected and --otp-stdin has no further line")
			}
			return nil, failf(exitSetupOTP, "--otp-stdin: could not read the two-factor code: %s", err)
		}
		return b, nil
	case len(s.envOTP) > 0:
		b := append([]byte(nil), s.envOTP...)
		wipe(s.envOTP)
		s.envOTP = nil
		return b, nil
	case try > 1 && !s.io.isTTY:
		return nil, failf(exitSetupOTP, "the two-factor code from %s was rejected; get a fresh code and run setup again", setupOTPEnv)
	case s.io.isTTY && !s.stdinUsed:
		b, err := s.io.readHidden("Two-factor code: ")
		if err != nil {
			return nil, failf(exitSetupOTP, "could not read the two-factor code: %s", err)
		}
		return b, nil
	}
	return nil, failf(exitSetupOTP, "two-factor authentication is required for this user, but no code source is available: set %s or pass --otp-stdin", setupOTPEnv)
}

// login logs in with PASSWORD_PLAIN (and OTP_TOKEN when asked) and returns
// the normalized user name and the user's roles (nil if not reported).
func (s *setup) login() (string, []string, *setupError) {
	if s.f.user == "" {
		u, err := s.io.readVisible("TrueNAS user name: ")
		if err != nil || strings.TrimSpace(u) == "" {
			return "", nil, failf(exitSetupUsage, "no user name given; pass --user")
		}
		s.f.user = strings.TrimSpace(u)
	}
	pw, fromTTY, err := s.password()
	if err != nil {
		return "", nil, err
	}
	defer func() { wipe(pw) }()

	reprompted := false
	for {
		s.sess.AddSecret(string(pw))
		raw, cerr := s.callNAS(s.sess, "auth.login_ex", map[string]interface{}{
			"mechanism":     "PASSWORD_PLAIN",
			"username":      s.f.user,
			"password":      string(pw),
			"login_options": map[string]interface{}{"user_info": true},
		})
		if cerr != nil {
			return "", nil, s.loginCallError(cerr)
		}
		resp, perr := parseLogin(raw)
		if perr != nil {
			return "", nil, perr
		}
		if resp.ResponseType == "OTP_REQUIRED" {
			resp, perr = s.otpLogin()
			if perr != nil {
				return "", nil, perr
			}
		}
		switch resp.ResponseType {
		case "SUCCESS":
			name, roles := s.f.user, []string(nil)
			if ui := resp.UserInfo; ui != nil {
				if ui.Username != "" {
					name = ui.Username
				}
				roles = ui.Privilege.Roles
				for _, a := range ui.AccountAttributes {
					if a == "PASSWORD_CHANGE_REQUIRED" {
						fprintf(s.io.stderr, "Note: TrueNAS asks %s to change the password soon; do that in the web UI.\n", name)
					}
				}
			}
			return name, roles, nil
		case "AUTH_ERR":
			if fromTTY && !reprompted {
				reprompted = true
				fprintln(s.io.stderr, "Login failed: wrong user name or password. Try again.")
				wipe(pw)
				b, rerr := s.io.readHidden(fmt.Sprintf("Password for %s: ", s.f.user))
				if rerr != nil {
					return "", nil, failf(exitSetupAuth, "could not read the password: %s", rerr)
				}
				pw = b
				continue
			}
			return "", nil, failf(exitSetupAuth, "login failed: wrong user name or password (TrueNAS answered AUTH_ERR). If GPOS STIG mode is on, password logins also need two-factor authentication")
		default:
			return "", nil, s.loginOutcome(resp)
		}
	}
}

// otpLogin continues an OTP_REQUIRED login with up to maxOTPTries codes.
func (s *setup) otpLogin() (*loginResponse, *setupError) {
	for try := 1; try <= maxOTPTries; try++ {
		code, err := s.otp(try)
		if err != nil {
			return nil, err
		}
		s.sess.AddSecret(string(code))
		raw, cerr := s.callNAS(s.sess, "auth.login_ex_continue", map[string]interface{}{
			"mechanism":     "OTP_TOKEN",
			"otp_token":     string(code),
			"login_options": map[string]interface{}{"user_info": true},
		})
		wipe(code)
		if cerr != nil {
			se := s.loginCallError(cerr)
			if se.code == exitSetupAuth {
				se.code = exitSetupOTP
			}
			return nil, se
		}
		resp, perr := parseLogin(raw)
		if perr != nil {
			return nil, perr
		}
		switch resp.ResponseType {
		case "OTP_REQUIRED":
			fprintln(s.io.stderr, "The two-factor code was rejected.")
			continue
		case "AUTH_ERR":
			return nil, failf(exitSetupOTP, "two-factor login failed: TrueNAS rejected the code too many times (AUTH_ERR)")
		}
		return resp, nil
	}
	return nil, failf(exitSetupOTP, "two-factor login failed: %d codes were rejected", maxOTPTries)
}

func parseLogin(raw json.RawMessage) (*loginResponse, *setupError) {
	var r loginResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, failf(exitSetupAuth, "could not read the auth.login_ex response")
	}
	return &r, nil
}

func (s *setup) loginOutcome(r *loginResponse) *setupError {
	switch r.ResponseType {
	case "EXPIRED":
		return failf(exitSetupAuth, "the password for %s has expired or must be changed: sign in to the TrueNAS web UI as that user, change it, then run setup again", s.f.user)
	case "REDIRECT":
		msg := "this TrueNAS controller is the standby node: run setup against the active controller or the virtual IP of the HA pair"
		if len(r.URLs) > 0 {
			msg += " (TrueNAS suggests: " + printable(strings.Join(r.URLs, ", ")) + ")"
		}
		return failf(exitSetupAuth, "%s", msg)
	}
	return failf(exitSetupAuth, "login failed: unexpected response_type %q", printable(r.ResponseType))
}

func (s *setup) loginCallError(err error) *setupError {
	switch apiErrno(err) {
	case errnoEOPNOTSUPP:
		return failf(exitSetupAuth, "TrueNAS does not allow this login mechanism at its current security level (GPOS STIG mode or a raised authenticator assurance level): %s", shortErr(err))
	case errnoEACCES:
		return failf(exitSetupPermission, "login refused: %s", shortErr(err))
	}
	var ce *truenas.ConnectError
	if errors.As(err, &ce) {
		return failf(exitSetupConnect, "%s", shortErr(err))
	}
	return failf(exitSetupAuth, "login failed: %s", shortErr(err))
}

func (s *setup) checkVersion(sess *truenas.Session) (truenas.Version, *setupError) {
	raw, err := s.callNAS(sess, "system.info")
	if err != nil {
		return truenas.Version{}, failf(exitSetupFailure, "system.info failed: %s", shortErr(err))
	}
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Version == "" {
		return truenas.Version{}, failf(exitSetupVersion, "could not read the TrueNAS version from system.info; TrueNAS %s or later is required", truenas.MinSupported)
	}
	v, verr := truenas.ParseVersion(info.Version)
	if verr != nil || v.Less(truenas.MinSupported) {
		return truenas.Version{}, failf(exitSetupVersion, "TrueNAS %s is not supported; TrueNAS %s or later is required", printable(info.Version), truenas.MinSupported)
	}
	return v, nil
}

// checkOwner makes sure the key owner exists and has a role, and asks for
// confirmation when it has (or may have) FULL_ADMIN.
func (s *setup) checkOwner(owner, loginName string, loginRoles []string) *setupError {
	roles, known := loginRoles, loginRoles != nil
	if owner != loginName {
		raw, err := s.callNAS(s.sess, "user.query", []interface{}{[]interface{}{"username", "=", owner}}, map[string]interface{}{"select": []string{"username", "roles"}})
		switch {
		case err == nil:
			var users []struct {
				Roles []string `json:"roles"`
			}
			if json.Unmarshal(raw, &users) != nil {
				return failf(exitSetupFailure, "could not read the user.query response")
			}
			if len(users) == 0 {
				return failf(exitSetupFailure, "user %s does not exist on TrueNAS. Create it in the web UI (Credentials > Users) with a limited role, then run setup again; setup does not create users", owner)
			}
			roles, known = users[0].Roles, true
		case apiErrno(err) == errnoEACCES:
			known = false
		default:
			return failf(exitSetupFailure, "could not look up user %s: %s", owner, shortErr(err))
		}
	}
	if known && len(roles) == 0 {
		return failf(exitSetupPermission, "user %s has no TrueNAS role, so TrueNAS will not create an API key for it; give its group a role (for example Read-Only Administrator) in the web UI", owner)
	}
	full := !known
	for _, r := range roles {
		if r == "FULL_ADMIN" {
			full = true
		}
	}
	if !full {
		return nil
	}
	w := s.io.stderr
	if known {
		fprintf(w, "WARNING: %s has FULL_ADMIN. An API key for it can do anything on the NAS, and so can any MCP client that uses it.\n", owner)
	} else {
		fprintf(w, "WARNING: could not read the roles of %s; it may have FULL_ADMIN, which would let the key do anything on the NAS.\n", owner)
	}
	fprintln(w, "Recommended: create a dedicated user with a limited role (for example Read-Only Administrator) in the web UI and pass --key-owner <user>.")
	if s.f.yesFullAdmin {
		return nil
	}
	if !s.io.isTTY || s.stdinUsed {
		return failf(exitSetupPermission, "refusing to create a full-admin key without confirmation; pass --yes-full-admin to allow it, or use --key-owner with a limited user")
	}
	ans, err := s.io.readVisible(fmt.Sprintf("Type the user name (%s) to create a full-admin key anyway: ", owner))
	if err != nil || strings.TrimSpace(ans) != owner {
		return failf(exitSetupPermission, "not confirmed; no key was created")
	}
	return nil
}

type apiKeyEntry struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Key      string `json:"key"`
}

// existingKeys lists API keys by name. api_key.query sees every user's
// keys (names are unique across users); api_key.my_keys is the fallback
// when the login user may not query them all.
// partial is true when only the login user's own keys could be listed.
func (s *setup) existingKeys(owner, loginName string) (keys []apiKeyEntry, partial bool, _ *setupError) {
	raw, err := s.callNAS(s.sess, "api_key.query")
	if err != nil && apiErrno(err) == errnoEACCES && owner == loginName {
		partial = true
		raw, err = s.callNAS(s.sess, "api_key.my_keys")
	}
	if err != nil {
		if apiErrno(err) == errnoEACCES {
			return nil, false, failf(exitSetupPermission, "cannot list API keys: %s", shortErr(err))
		}
		return nil, false, failf(exitSetupFailure, "cannot list API keys: %s", shortErr(err))
	}
	if json.Unmarshal(raw, &keys) != nil {
		return nil, false, failf(exitSetupFailure, "could not read the API key list")
	}
	return keys, partial, nil
}

func (s *setup) expiresParam() interface{} {
	if s.expiresAt == nil {
		return nil
	}
	return mwtime.Date(*s.expiresAt)
}

// createKey creates the key (or resets it with --rotate) and returns it
// with the secret, which is kept in memory only.
func (s *setup) createKey(owner, loginName string) (*apiKeyEntry, *setupError) {
	keys, partial, err := s.existingKeys(owner, loginName)
	if err != nil {
		return nil, err
	}
	taken := map[string]*apiKeyEntry{}
	for i := range keys {
		taken[keys[i].Name] = &keys[i]
	}

	if s.f.rotate {
		if k := taken[s.f.name]; k != nil {
			if k.Username != owner {
				return nil, failf(exitSetupPermission, "the API key %q belongs to %s, not %s; --rotate only resets a key owned by the key owner", s.f.name, printable(k.Username), owner)
			}
			raw, cerr := s.callNAS(s.sess, "api_key.update", k.ID, map[string]interface{}{"reset": true, "expires_at": s.expiresParam()})
			if cerr != nil {
				return nil, s.keyCallError(cerr, "reset")
			}
			return s.parseKey(raw)
		}
		fprintf(s.io.stderr, "No API key named %s exists yet; creating it.\n", s.f.name)
	}

	blindRetries := 0
	for n := 1; n <= maxNameSuffix; n++ {
		name := s.f.name
		if n > 1 {
			name = fmt.Sprintf("%s-%d", s.f.name, n)
		}
		if taken[name] != nil {
			continue
		}
		raw, cerr := s.callNAS(s.sess, "api_key.create", map[string]interface{}{
			"name":       name,
			"username":   owner,
			"expires_at": s.expiresParam(),
		})
		if cerr != nil {
			// Names are unique across users, and api_key.my_keys does not
			// show other users' keys, so a validation error may be a name
			// collision. Its text is redacted, so try a few more names.
			if partial && apiErrno(cerr) == errnoEINVAL && blindRetries < maxBlindRetries {
				blindRetries++
				continue
			}
			return nil, s.keyCallError(cerr, "create")
		}
		return s.parseKey(raw)
	}
	return nil, failf(exitSetupFailure, "API key names %s to %s-%d are all taken; pass --name", s.f.name, s.f.name, maxNameSuffix)
}

func (s *setup) parseKey(raw json.RawMessage) (*apiKeyEntry, *setupError) {
	var k apiKeyEntry
	if json.Unmarshal(raw, &k) != nil || k.Key == "" || k.ID == 0 {
		return nil, failf(exitSetupFailure, "TrueNAS did not return the new API key")
	}
	s.sess.AddSecret(k.Key)
	return &k, nil
}

func (s *setup) keyCallError(err error, action string) *setupError {
	msg := shortErr(err)
	if apiErrno(err) == errnoEACCES {
		if strings.Contains(msg, "STIG") {
			return failf(exitSetupPermission, "TrueNAS is in GPOS STIG mode, which does not allow API keys to be created or changed (and does not allow API key logins), so the MCP server cannot be set up with a key on this system")
		}
		return failf(exitSetupPermission, "TrueNAS refused to %s the API key: %s", action, msg)
	}
	return failf(exitSetupFailure, "could not %s the API key: %s", action, msg)
}

// save writes the pin (when confirmed in this run) and the key file. If
// that fails, the key is deleted from TrueNAS so no usable key is left
// behind that nobody holds.
func (s *setup) save(k *apiKeyEntry) *setupError {
	err := ensureConfigDir(filepath.Dir(s.keyPath))
	if err == nil && s.savePin {
		err = writeFileAtomic(s.pinPath, []byte(s.pin+"\n"), true)
	}
	if err == nil {
		data := append([]byte(k.Key), '\n')
		err = writeFileAtomic(s.keyPath, data, s.f.force || s.f.rotate)
		wipe(data)
	}
	if err == nil {
		return nil
	}
	reason := err.Error()
	if errors.Is(err, errFileExists) {
		reason = s.keyPath + " already exists (pass --force to replace it)"
	}
	if _, derr := s.callNAS(s.sess, "api_key.delete", k.ID); derr != nil {
		return failf(exitSetupIO, "could not save the API key: %s. Deleting the key from TrueNAS also failed (%s): delete the key %q (id %d) in the web UI", reason, shortErr(derr), k.Name, k.ID)
	}
	return failf(exitSetupIO, "could not save the API key: %s. The key was deleted from TrueNAS; fix the problem and run setup again", reason)
}

// verify logs in on a fresh connection with the new key and reads
// system.info.
func (s *setup) verify(owner, key string) *setupError {
	sess, err := s.dial()
	if err != nil {
		se := s.connectError(err)
		se.code = exitSetupVerify
		se.msg = "could not verify the new key: " + se.msg + ". " + s.keptNote()
		return se
	}
	defer sess.Close()
	sess.AddSecret(key)
	raw, err := s.callNAS(sess, "auth.login_ex", map[string]interface{}{
		"mechanism":     "API_KEY_PLAIN",
		"username":      owner,
		"api_key":       key,
		"login_options": map[string]interface{}{"user_info": false},
	})
	if err != nil {
		return failf(exitSetupVerify, "the new key could not log in: %s. %s", shortErr(err), s.keptNote())
	}
	resp, perr := parseLogin(raw)
	if perr != nil || resp.ResponseType != "SUCCESS" {
		got := "an unreadable response"
		if resp != nil {
			got = printable(resp.ResponseType)
		}
		return failf(exitSetupVerify, "the new key could not log in (TrueNAS answered %s). %s", got, s.keptNote())
	}
	if _, err := s.callNAS(sess, "system.info"); err != nil {
		return failf(exitSetupVerify, "the new key logged in but system.info failed: %s. %s", shortErr(err), s.keptNote())
	}
	return nil
}

func (s *setup) keptNote() string {
	return "The key file was kept at " + s.keyPath + "; if you do not want it, delete the key in the TrueNAS web UI and remove the file"
}

func (s *setup) report(owner string, k *apiKeyEntry, v truenas.Version) *setupError {
	w := s.io.stderr
	expires := "never"
	if s.expiresAt != nil {
		expires = s.expiresAt.Format(time.DateOnly)
	}
	fprintf(w, "Saved the API key to %s (mode 0600).\n", s.keyPath)
	fprintf(w, "Key %q (id %d), owner %s, expires %s.\n", k.Name, k.ID, owner, expires)
	if s.savePin {
		fprintf(w, "Pinned the TrueNAS certificate %s in %s.\n", s.pin, s.pinPath)
	}
	fprintf(w, "Verified: the new key logged in and read system.info (TrueNAS %s).\n", v)
	fprintf(w, "To revoke it: in the TrueNAS web UI open Credentials > Users > %s > View API Keys (or the user menu > My API Keys when signed in as %s), delete %q, then delete %s.\n", owner, owner, k.Name, s.keyPath)

	env := map[string]string{
		"TRUENAS_URL":          s.endpointHost,
		"TRUENAS_API_USER":     owner,
		"TRUENAS_API_KEY_FILE": s.keyPath,
	}
	switch {
	case s.pin != "":
		env["TRUENAS_TLS_PIN"] = s.pin
	case s.f.tlsCA != "":
		env["TRUENAS_TLS_CA"] = s.f.tlsCA
	case s.f.insecure:
		env["TRUENAS_INSECURE"] = "1"
		fprintln(w, "WARNING: the configuration below disables certificate verification. Prefer pinning: run setup again without --insecure.")
	}
	if s.f.proxy != "" {
		env["TRUENAS_PROXY"] = s.f.proxy
	}
	cmd, err := s.io.executable()
	if err != nil || cmd == "" {
		cmd = "truenas-mcp"
	}
	snippet, err := clientSnippet(s.f.printConfig, cmd, env)
	if err != nil {
		return failf(exitSetupUsage, "%s", err)
	}
	fprintf(w, "\n%s\n", snippetHint(s.f.printConfig))
	fprint(s.io.stdout, snippet)
	return nil
}
