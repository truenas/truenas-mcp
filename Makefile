.PHONY: build build-linux clean test lint run deploy build-proxy build-proxy-darwin build-proxy-linux build-proxy-windows build-proxy-all

BINARY_NAME=truenas-mcp
PROXY_BINARY_NAME=truenas-mcp-proxy
BUILD_DIR=.
TARGET_HOST=root@10.220.171.151
TARGET_PATH=/usr/local/bin/truenas-mcp

# Build server for local platform
build:
	@echo "Building $(BINARY_NAME) for local platform..."
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/truenas-mcp

# Cross-compile server for Linux x86_64
build-linux:
	@echo "Building $(BINARY_NAME) for Linux x86_64..."
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/truenas-mcp

# Build proxy for local platform
build-proxy:
	@echo "Building $(PROXY_BINARY_NAME) for local platform..."
	go build -o $(BUILD_DIR)/$(PROXY_BINARY_NAME) ./cmd/truenas-mcp-proxy

# Cross-compile proxy for macOS (both architectures)
build-proxy-darwin:
	@echo "Building $(PROXY_BINARY_NAME) for macOS amd64..."
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(PROXY_BINARY_NAME)-darwin-amd64 ./cmd/truenas-mcp-proxy
	@echo "Building $(PROXY_BINARY_NAME) for macOS arm64..."
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(PROXY_BINARY_NAME)-darwin-arm64 ./cmd/truenas-mcp-proxy

# Cross-compile proxy for Linux
build-proxy-linux:
	@echo "Building $(PROXY_BINARY_NAME) for Linux amd64..."
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(PROXY_BINARY_NAME)-linux-amd64 ./cmd/truenas-mcp-proxy

# Cross-compile proxy for Windows
build-proxy-windows:
	@echo "Building $(PROXY_BINARY_NAME) for Windows amd64..."
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(PROXY_BINARY_NAME)-windows-amd64.exe ./cmd/truenas-mcp-proxy

# Build all proxy platforms
build-proxy-all: build-proxy-darwin build-proxy-linux build-proxy-windows

clean:
	@echo "Cleaning..."
	rm -f $(BUILD_DIR)/$(BINARY_NAME)
	rm -f $(BUILD_DIR)/$(PROXY_BINARY_NAME)*

test:
	@echo "Running tests..."
	go test -v ./...

lint:
	@echo "Running linters..."
	go vet ./...
	go fmt ./...

run: build
	@echo "Running $(BINARY_NAME)..."
	./$(BINARY_NAME)

# Deploy to TrueNAS test system
deploy: build-linux
	@echo "Deploying to $(TARGET_HOST)..."
	scp $(BUILD_DIR)/$(BINARY_NAME) $(TARGET_HOST):$(TARGET_PATH)
	ssh $(TARGET_HOST) 'chmod +x $(TARGET_PATH)'
	@echo "Deployed to $(TARGET_PATH)"

# Test connection to TrueNAS
test-remote:
	@echo "Testing connection to TrueNAS..."
	ssh $(TARGET_HOST) '$(TARGET_PATH) --version || echo "Binary not found or not executable"'
