package main

import (
	"flag"
	"log"
	"os"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tools"
	"github.com/truenas/truenas-mcp/truenas"
)

var (
	listenAddr      = flag.String("listen", "localhost:8080", "Listen address (host:port)")
	apiKey          = flag.String("api-key", "", "API key for MCP client authentication (required)")
	truenasAPIKey   = flag.String("truenas-api-key", "", "TrueNAS API key for middleware authentication (required)")
	version         = flag.Bool("version", false, "Print version and exit")
)

const (
	Version = "0.1.0"
)

func main() {
	flag.Parse()

	if *version {
		log.Printf("truenas-mcp version %s", Version)
		os.Exit(0)
	}

	// API key is required for security
	if *apiKey == "" {
		apiKey = new(string)
		*apiKey = os.Getenv("TRUENAS_MCP_API_KEY")
		if *apiKey == "" {
			log.Println("WARNING: No API key specified via -api-key flag or TRUENAS_MCP_API_KEY env var.")
			log.Println("Authentication is disabled. This is not recommended for production.")
		}
	}

	// TrueNAS API key is required for middleware authentication
	if *truenasAPIKey == "" {
		truenasAPIKey = new(string)
		*truenasAPIKey = os.Getenv("TRUENAS_API_KEY")
		if *truenasAPIKey == "" {
			log.Fatal("TrueNAS API key required via -truenas-api-key flag or TRUENAS_API_KEY env var")
		}
	}

	// Initialize TrueNAS client
	socketPath := os.Getenv("TRUENAS_SOCKET")
	if socketPath == "" {
		socketPath = "/var/run/middleware/middlewared.sock"
	}

	client, err := truenas.NewClient(socketPath, *truenasAPIKey)
	if err != nil {
		log.Fatalf("Failed to create TrueNAS client: %v", err)
	}

	// Authenticate with TrueNAS middleware
	if err := client.Authenticate(); err != nil {
		log.Fatalf("Failed to authenticate with TrueNAS: %v", err)
	}
	log.Println("Successfully authenticated with TrueNAS middleware")

	// Initialize tool registry with TrueNAS client
	registry := tools.NewRegistry(client)

	// Start SSE server
	log.Printf("Starting TrueNAS MCP Server v%s on %s...", Version, *listenAddr)
	server := mcp.NewSSEServer(registry, *listenAddr, *apiKey)
	if err := server.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
