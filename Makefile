.PHONY: build test lint clean run help

# Build variables
BINARY_NAME := session-proxy
BUILD_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/session-proxy

## test: Run all tests
test:
	go test ./... -v -count=1

## test-short: Run tests without verbose output
test-short:
	go test ./... -count=1

## lint: Run golangci-lint
lint:
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run

## fmt: Format code
fmt:
	go fmt ./...
	goimports -w .

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f session-proxy
	rm -f debug.log

## run: Build and run with example config
run: build
	$(BUILD_DIR)/$(BINARY_NAME) --config config.example.yaml --debug

## install: Install to GOPATH/bin
install:
	go install $(LDFLAGS) ./cmd/session-proxy

## deps: Download dependencies
deps:
	go mod download
	go mod tidy

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/ /'

# Default target
.DEFAULT_GOAL := help
