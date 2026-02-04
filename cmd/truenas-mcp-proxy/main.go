package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/truenas/truenas-mcp/proxy"
)

const (
	Version = "0.1.0"
)

func main() {
	// Load configuration
	config, err := proxy.LoadConfig()
	if err != nil {
		if err.Error() == "version requested" {
			log.Printf("truenas-mcp-proxy version %s", Version)
			os.Exit(0)
		}
		log.Fatalf("Configuration error: %v", err)
	}

	if config.Debug {
		log.Printf("truenas-mcp-proxy v%s starting...", Version)
		log.Printf("Server URL: %s", config.ServerURL)
		log.Printf("Timeout: %s", config.Timeout)
		if config.Insecure {
			log.Printf("WARNING: TLS certificate verification disabled")
		}
	}

	// Create proxy
	p := proxy.NewProxy(config)

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start proxy in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- p.Run()
	}()

	// Wait for completion or signal
	select {
	case err := <-errChan:
		if err != nil {
			log.Fatalf("Proxy error: %v", err)
		}
	case sig := <-sigChan:
		if config.Debug {
			log.Printf("Received signal: %v", sig)
		}
		p.Shutdown()
	}

	if config.Debug {
		log.Printf("Proxy shutdown complete")
	}
}
