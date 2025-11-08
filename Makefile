# Makefile for sdhm Go project

# Colors for output
GREEN=\033[0;32m
NC=\033[0m # No Color

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt
GOVET=$(GOCMD) vet
BINARY_NAME=sdhm
BINARY_PATH=./cmd/sdhm
BUILD_DIR=build
VERSION?=0.0.0.0-dev

# Build flags
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean test coverage deps update tidy modernize fmt vet lint help run install

# Default target
all: test build

## build: Build the sdhm binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(BINARY_PATH)

## clean: Clean build artifacts and test files
clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

## test: Run all tests (doesn't touch production /etc/hosts)
test:
	@echo "Running tests..."
	@echo "Note: Tests use temporary files and do not modify production /etc/hosts"
	$(GOTEST) -v ./...

## test-short: Run short tests
test-short:
	@echo "Running short tests..."
	$(GOTEST) -short -v ./...

## test-coverage: Run tests with coverage report
test-coverage:
	@echo "Running tests with coverage..."
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"
	@$(GOCMD) tool cover -func=coverage.out | grep total | awk '{print "Total coverage: " $$3}'

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download

## update: Update dependencies to latest versions
update:
	@echo "Updating dependencies..."
	$(GOGET) -u ./...
	$(GOMOD) tidy

## tidy: Tidy go.mod
tidy:
	@echo "Tidying go.mod..."
	$(GOMOD) tidy

## modernize: Modernize the project (format, vet, update, tidy, and apply latest Go patterns)
modernize: fmt vet update tidy
	@echo "$(GREEN)Running Go modernization tool...$(NC)"
	@$(GOCMD) run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix -test ./... || echo "Note: modernize tool completed (warnings are normal)"
	@echo "$(GREEN)Project modernized!$(NC)"

## fmt: Format code
fmt:
	@echo "Formatting code..."
	$(GOFMT) ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	$(GOVET) ./...

## lint: Run golangci-lint (requires golangci-lint to be installed)
lint:
	@echo "Running golangci-lint..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed. Install with: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin"; \
	fi

## run: Run the application with example interval (requires root for /etc/hosts)
run:
	@echo "Running $(BINARY_NAME) with 5m interval..."
	@echo "Note: This requires root access to modify /etc/hosts"
	$(GOCMD) run $(BINARY_PATH) -interval 5m

## install: Install the binary to /usr/local/bin (requires root)
install: build
	@echo "Installing $(BINARY_NAME) to /usr/local/bin..."
	@sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/
	@sudo chmod 755 /usr/local/bin/$(BINARY_NAME)
	@echo "Installed successfully!"

## uninstall: Remove the binary from /usr/local/bin
uninstall:
	@echo "Uninstalling $(BINARY_NAME)..."
	@sudo rm -f /usr/local/bin/$(BINARY_NAME)
	@echo "Uninstalled successfully!"

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Available targets:"
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
