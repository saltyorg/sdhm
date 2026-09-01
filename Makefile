# Makefile for sdhm Go project

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
VERSION?=0.0.0-dev
TEST_PACKAGES ?= ./...
TEST_FLAGS ?=

# Build flags
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean test test-short test-coverage deps update tidy fmt vet help run install uninstall fmt-check tidy-check test-race check

# Default target
## all: Run tests and build the binary
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
	$(GOTEST) $(TEST_FLAGS) $(TEST_PACKAGES)

## fmt-check: Check that tracked Go files are formatted
fmt-check:
	@diff="$$(git ls-files -z --cached -- '*.go' | \
		xargs -0 -r sh -c 'for file do \
			[ ! -e "$$file" ] && continue; \
			gofmt -d "$$file"; status=$$?; \
			[ "$$status" -le 1 ] || exit "$$status"; \
		done' sh)"; \
	status=$$?; \
	if [ "$$status" -ne 0 ]; then exit "$$status"; fi; \
	if [ -n "$$diff" ]; then printf '%s\n' "$$diff"; exit 1; fi

## tidy-check: Check module files without modifying them
tidy-check:
	$(GOMOD) tidy -diff

## test-race: Run all tests with the race detector
test-race:
	$(GOTEST) -race $(TEST_FLAGS) $(TEST_PACKAGES)

## check: Run formatting, module, race, vet, and build gates
check: fmt-check tidy-check test-race vet build

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

## fmt: Format code
fmt:
	@echo "Formatting code..."
	$(GOFMT) ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	$(GOVET) ./...

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
