package proxy

import (
	"errors"
	"flag"
	"os"
	"time"
)

// Config holds proxy configuration
type Config struct {
	ServerURL string
	APIKey    string
	Timeout   time.Duration
	Debug     bool
	Insecure  bool
}

// LoadConfig loads configuration from flags and environment variables
func LoadConfig() (*Config, error) {
	cfg := &Config{}

	// Define flags
	serverURL := flag.String("server-url", "", "TrueNAS MCP server URL (e.g., http://192.168.0.31:8089)")
	apiKey := flag.String("api-key", "", "API key for authentication")
	timeout := flag.Duration("timeout", 30*time.Second, "Request timeout")
	debug := flag.Bool("debug", false, "Enable debug logging")
	insecure := flag.Bool("insecure", false, "Skip TLS certificate verification (not recommended)")
	version := flag.Bool("version", false, "Print version and exit")

	flag.Parse()

	// Handle version flag
	if *version {
		return nil, errors.New("version requested")
	}

	// Load from flags or environment variables
	cfg.ServerURL = *serverURL
	if cfg.ServerURL == "" {
		cfg.ServerURL = os.Getenv("TRUENAS_MCP_SERVER_URL")
	}

	cfg.APIKey = *apiKey
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("TRUENAS_MCP_API_KEY")
	}

	cfg.Timeout = *timeout
	cfg.Debug = *debug
	cfg.Insecure = *insecure

	// Validate required fields
	if cfg.ServerURL == "" {
		return nil, errors.New("server URL is required (use --server-url or TRUENAS_MCP_SERVER_URL)")
	}

	if cfg.APIKey == "" {
		return nil, errors.New("API key is required (use --api-key or TRUENAS_MCP_API_KEY)")
	}

	return cfg, nil
}
