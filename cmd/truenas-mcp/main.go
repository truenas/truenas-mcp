package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	runtimedebug "runtime/debug"
	"strings"
	"time"

	// Embed IANA zoneinfo so time.LoadLocation works on Windows (and other
	// hosts without a system zone database) for NAS timezone next_run.
	_ "time/tzdata"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/tools"
	"github.com/truenas/truenas-mcp/truenas"
)

var (
	truenasURL          = flag.String("truenas-url", "", "TrueNAS hostname or WebSocket URL (e.g., 'truenas.example.com' or 'wss://10.0.0.1/api/current')")
	apiUser             = flag.String("api-user", "", "User that owns the API key, sent to auth.login_ex (required; env TRUENAS_API_USER)")
	apiKey              = flag.String("api-key", "", "TrueNAS API key for middleware authentication (env TRUENAS_API_KEY; visible in the process list)")
	apiKeyFile          = flag.String("api-key-file", "", "Path to a file containing the TrueNAS API key (env TRUENAS_API_KEY_FILE)")
	insecure            = flag.Bool("insecure", false, "Disable TLS certificate verification (UNSAFE: allows man-in-the-middle attacks; prefer --tls-ca)")
	tlsCA               = flag.String("tls-ca", "", "Path to a PEM certificate to trust (e.g., the TrueNAS self-signed certificate)")
	proxyURL            = flag.String("proxy", "", "Proxy for the TrueNAS connection: http://, socks5:// or socks5h://host:port, or none to ignore proxy env vars (env TRUENAS_PROXY; overrides HTTPS_PROXY and ALL_PROXY)")
	versionFlg          = flag.Bool("version", false, "Print version and exit")
	debug               = flag.Bool("debug", false, "Enable debug logging")
	readOnly            = flag.Bool("read-only", false, "Hide and refuse every tool that changes the system (env TRUENAS_MCP_READ_ONLY)")
	requireHumanConfirm = flag.Bool("require-human-confirm", false, "Refuse destructive tools on clients that do not support MCP elicitation, instead of falling back to a confirmation token (env TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM)")
)

// --allow-tools and --deny-tools are read through flag.Visit in main (a passed
// flag wins over its environment variable), so they need no variable here.
func init() {
	flag.String("allow-tools", "", "Comma-separated list of the only tools to expose (env TRUENAS_MCP_ALLOW_TOOLS)")
	flag.String("deny-tools", "", "Comma-separated list of tools to hide and refuse (env TRUENAS_MCP_DENY_TOOLS)")
}

// Version is the release version reported by --version and in the MCP
// serverInfo. Release builds (make, CI) inject it at build time with
// -ldflags "-X main.Version=..." from the git tag or git describe. Without
// injection it falls back to the module version Go records in the binary
// (set by go install module@version, and by go build in a tagged git
// checkout), and otherwise to "dev".
var Version = "dev"

func init() {
	info, ok := runtimedebug.ReadBuildInfo()
	Version = resolveVersion(Version, info, ok)
}

// resolveVersion returns the injected version unless it is empty or "dev",
// then the main module version from the build info without its "v", and
// finally "dev".
func resolveVersion(injected string, info *runtimedebug.BuildInfo, ok bool) string {
	if injected != "" && injected != "dev" {
		return injected
	}
	if ok && info != nil {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}

// checkRequired reports any required setting that is missing.
func checkRequired(truenasURL, apiKey, apiUser string) error {
	var missing []string
	if truenasURL == "" {
		missing = append(missing, "--truenas-url (TRUENAS_URL)")
	}
	if apiKey == "" {
		missing = append(missing, "--api-key (TRUENAS_API_KEY, or --api-key-file / TRUENAS_API_KEY_FILE)")
	}
	if apiUser == "" {
		missing = append(missing, "--api-user (TRUENAS_API_USER, the user that owns the API key)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

// truthy reports whether an environment value enables a boolean setting.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// safetyPolicy combines the safety flags with their environment variables.
// setFlags holds the flags passed on the command line; a passed flag wins
// over its variable. An allow list that is set but names no tools is an
// error rather than "allow everything".
func safetyPolicy(readOnly, requireHumanConfirm bool, setFlags map[string]string, lookupEnv func(string) (string, bool)) (tools.Policy, error) {
	setting := func(flagName, envName string) (string, bool) {
		if v, ok := setFlags[flagName]; ok {
			return v, true
		}
		return lookupEnv(envName)
	}
	roEnv, _ := lookupEnv("TRUENAS_MCP_READ_ONLY")
	rhcEnv, _ := lookupEnv("TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM")
	allow, allowSet := setting("allow-tools", "TRUENAS_MCP_ALLOW_TOOLS")
	deny, _ := setting("deny-tools", "TRUENAS_MCP_DENY_TOOLS")
	p := tools.Policy{
		ReadOnly:            readOnly || truthy(roEnv),
		RequireHumanConfirm: requireHumanConfirm || truthy(rhcEnv),
		Allow:               tools.ParseToolList(allow),
		Deny:                tools.ParseToolList(deny),
	}
	if allowSet && len(p.Allow) == 0 {
		return p, fmt.Errorf("--allow-tools / TRUENAS_MCP_ALLOW_TOOLS is set but names no tools; unset it to expose all tools")
	}
	return p, nil
}

// configureLogging sends the standard logger to stderr. Stdout carries the
// JSON-RPC stream, so a log line there would corrupt the protocol framing.
// This is the log package's default; setting it explicitly keeps it true
// even if a dependency or a later change alters the default.
func configureLogging() {
	log.SetOutput(os.Stderr)
}

func main() {
	configureLogging()
	flag.Parse()

	if *versionFlg {
		fmt.Printf("truenas-mcp version %s\n", Version)
		os.Exit(0)
	}

	// Get configuration from flags or environment variables
	if *truenasURL == "" {
		*truenasURL = os.Getenv("TRUENAS_URL")
	}
	if *apiUser == "" {
		*apiUser = os.Getenv("TRUENAS_API_USER")
	}
	key, err := resolveAPIKey(*apiKey, *apiKeyFile, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	*apiKey = key

	if err := checkRequired(*truenasURL, *apiKey, *apiUser); err != nil {
		log.Fatal(err)
	}

	// TRUENAS_MCP_DEBUG=1 is equivalent to --debug; either enables verbose
	// (credential-redacted) request/response logging everywhere
	if truenas.DebugLogging() {
		*debug = true
	}
	truenas.SetDebugLogging(*debug)

	if *tlsCA == "" {
		*tlsCA = os.Getenv("TRUENAS_TLS_CA")
	}
	if !*insecure {
		switch strings.ToLower(os.Getenv("TRUENAS_INSECURE")) {
		case "", "0", "false", "no", "off":
		default:
			*insecure = true
		}
	}

	// Configure TLS: certificates are verified by default. TrueNAS ships a
	// self-signed certificate, so users must either trust it via --tls-ca
	// or explicitly opt out of verification with --insecure.
	tlsConfig, err := truenas.NewTLSConfig(*insecure, *tlsCA)
	if err != nil {
		log.Fatalf("Failed to configure TLS: %v", err)
	}
	if *insecure {
		log.Println(insecureWarning(*tlsCA != ""))
	}

	// Create TrueNAS client
	client, err := truenas.NewClientWithOptions(*truenasURL, *apiKey, tlsConfig, truenas.Options{
		APIUser: *apiUser,
		Proxy:   *proxyURL,
	})
	if err != nil {
		log.Fatalf("Failed to create TrueNAS client: %v", err)
	}
	if err := client.ValidateEndpoint(); err != nil {
		log.Fatalf("%v (check --truenas-url / TRUENAS_URL)", err)
	}
	if err := client.ValidateProxy(); err != nil {
		log.Fatalf("Invalid proxy configuration: %v", err)
	}
	defer client.Close()

	// Authenticate with TrueNAS middleware
	if err := client.Authenticate(context.Background()); err != nil {
		log.Fatalf("%s", startupAuthError(err))
	}
	log.Println("Successfully authenticated with TrueNAS middleware")
	if err := client.RequireSupportedVersion(context.Background()); err != nil {
		log.Fatalf("%s", err)
	}

	// Create task manager
	taskConfig := tasks.PollerConfig{
		PollInterval:    5 * time.Second,
		CleanupInterval: 1 * time.Minute,
	}
	taskManager := tasks.NewManager(client, taskConfig)
	taskManager.Start()
	defer taskManager.Shutdown()

	// Create tool registry
	registry := tools.NewRegistry(client, taskManager)
	setFlags := map[string]string{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = f.Value.String() })
	policy, err := safetyPolicy(*readOnly, *requireHumanConfirm, setFlags, os.LookupEnv)
	if err != nil {
		log.Fatalf("Invalid tool safety settings: %v", err)
	}
	if err := registry.SetPolicy(policy); err != nil {
		log.Fatalf("Invalid tool safety settings: %v", err)
	}
	if policy.ReadOnly {
		log.Println("Read-only mode: tools that change the system are disabled (upgrade_app is available for dry runs only)")
	}
	if policy.RequireHumanConfirm {
		log.Println("Require-human-confirm: destructive tools are refused on clients that do not support MCP elicitation")
	}

	handler := NewStdioHandler(registry, *debug)
	if err := handler.Run(context.Background()); err != nil {
		log.Fatalf("Stdio handler error: %v", err)
	}
}

// insecureWarning is logged at startup when TLS verification is disabled.
// It points to --tls-ca, the safer way to use TrueNAS's self-signed
// certificate. caIgnored is true if a CA file was also given, which
// --insecure overrides.
func insecureWarning(caIgnored bool) string {
	msg := "WARNING: TLS certificate verification is disabled (--insecure / TRUENAS_INSECURE): the connection is vulnerable to man-in-the-middle attacks. " +
		"Prefer --tls-ca <cert.pem> (or TRUENAS_TLS_CA) to trust the server's certificate and keep verification on."
	if caIgnored {
		msg += " The CA file you gave is not used while --insecure is set."
	}
	return msg
}

// startupAuthError words the failure of the first connect and login. A
// connection that could not be established is not an authentication failure
// and says so; only a rejected or failed login is reported as one.
func startupAuthError(err error) string {
	var ee *truenas.EndpointError
	if errors.As(err, &ee) {
		return ee.Error()
	}
	var ce *truenas.ConnectError
	if errors.As(err, &ce) {
		if ce.LoginAttempted {
			return fmt.Sprintf("Could not connect to TrueNAS (the connection was lost during login, and reconnecting failed): %v", ce.Err)
		}
		return fmt.Sprintf("Could not connect to TrueNAS (no login was attempted): %v", ce.Err)
	}
	return fmt.Sprintf("Failed to authenticate with TrueNAS: %v", err)
}
