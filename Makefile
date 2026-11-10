.PHONY: build build-all clean test lint

BINARY_NAME=truenas-mcp
BUILD_DIR=.

# Release version: taken from the git tag (v stripped), e.g. v0.0.5 -> 0.0.5.
# Override with: make build VERSION=1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS = -ldflags "-X main.Version=$(VERSION)"

# Build for local platform
build:
	@echo "Building $(BINARY_NAME) $(VERSION) for local platform..."
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/truenas-mcp

# Build for all platforms
build-all:
	@echo "Building $(VERSION) for all platforms..."
	@echo "Building for macOS (ARM64)..."
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/truenas-mcp
	@echo "Building for macOS (AMD64)..."
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/truenas-mcp
	@echo "Building for Linux (AMD64)..."
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/truenas-mcp
	@echo "Building for Windows (AMD64)..."
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/truenas-mcp
	@echo "All builds complete!"

clean:
	@echo "Cleaning..."
	rm -f $(BUILD_DIR)/$(BINARY_NAME)
	rm -f $(BUILD_DIR)/$(BINARY_NAME)-*

test:
	@echo "Running tests..."
	go test -v -race ./...

# Pinned linter versions (Go 1.27 compatible). CI runs this same target.
STATICCHECK_VERSION ?= v0.8.2
ERRCHECK_VERSION ?= v1.30.0

lint:
	@echo "Running linters..."
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "Unformatted files (run gofmt -w):"; gofmt -l .; exit 1; }
	# ST1005/ST1008 are style checks: one would change a user-facing error
	# string, the other would churn test helpers that return (out, err, fake).
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -checks 'all,-ST1000,-ST1003,-ST1016,-ST1020,-ST1021,-ST1022,-ST1005,-ST1008' ./...
	go run github.com/kisielk/errcheck@$(ERRCHECK_VERSION) -ignoretests -exclude .errcheck-excludes ./...
