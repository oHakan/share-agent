.PHONY: build run clean install-tools

# Get version from git tags
VERSION ?= $(shell git describe --tags --always)
COMMIT ?= $(shell git rev-parse --short HEAD)
DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
DEV_MODE ?= true  # Local development mode (true), can be overridden

# Build paths
BINARY_NAME := vortix-agent
LDFLAGS := -ldflags "-s -w \
	-X github.com/depin-agent/agent/internal/config.Version=$(VERSION) \
	-X github.com/depin-agent/agent/internal/config.Commit=$(COMMIT) \
	-X github.com/depin-agent/agent/internal/config.Date=$(DATE) \
	-X github.com/depin-agent/agent/internal/config.DevMode=$(DEV_MODE)"

# Default target
all: build

# Build the agent
build:
	@echo "Building $(BINARY_NAME) v$(VERSION)..."
	@go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/agent
	@echo "✓ Build complete: bin/$(BINARY_NAME)"

# Build for multiple platforms (like goreleaser)
build-all:
	@echo "Building for multiple platforms v$(VERSION)..."
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-arm64 ./cmd/agent
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-amd64 ./cmd/agent
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-arm64 ./cmd/agent
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-windows-amd64.exe ./cmd/agent
	@echo "✓ Multi-platform build complete"

# Run the agent
run: build
	./bin/$(BINARY_NAME)

# Run with specific API key
run-with-key: build
	./bin/$(BINARY_NAME) --api-key="$(API_KEY)"

# Clean build artifacts
clean:
	@rm -rf bin/
	@go clean
	@echo "✓ Clean complete"

# Install dependencies
deps:
	@go mod download
	@go mod verify
	@echo "✓ Dependencies verified"

# Tidy go.mod
tidy:
	@go mod tidy
	@echo "✓ go.mod tidied"

# Run tests
test:
	@go test -v ./...

# Format code
fmt:
	@go fmt ./...
	@echo "✓ Code formatted"

# Lint code
lint:
	@which golangci-lint || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	@golangci-lint run ./...

# Show version info
version:
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Date: $(DATE)"

# Print help
help:
	@echo "Available targets:"
	@echo "  make build          - Build the agent (default)"
	@echo "  make build-all      - Build for multiple platforms"
	@echo "  make run            - Build and run the agent"
	@echo "  make run-with-key   - Run with API key (set API_KEY=...)"
	@echo "  make clean          - Remove build artifacts"
	@echo "  make deps           - Download and verify dependencies"
	@echo "  make tidy           - Run go mod tidy"
	@echo "  make test           - Run tests"
	@echo "  make fmt            - Format code"
	@echo "  make lint           - Run linter"
	@echo "  make version        - Show version info"
	@echo ""
	@echo "Example: make run API_KEY=your-key-here"
