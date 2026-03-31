BINARY        := dicode
DAEMON_BINARY := dicoded
CMD           := ./cmd/dicode
DAEMON_CMD    := ./cmd/dicoded
VERSION       ?= dev

GO      := $(shell which go 2>/dev/null || echo $(HOME)/.local/share/mise/shims/go)
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-verbose test-race lint fmt clean run tidy help test-e2e test-e2e-headed test-e2e-ui

## build: compile both dicode (CLI) and dicoded (daemon) into the project root
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)
	$(GO) build $(GOFLAGS) -o $(DAEMON_BINARY) $(DAEMON_CMD)

## run: build and run dicoded daemon (Ctrl-C to stop)
run: build
	./$(DAEMON_BINARY)

## test: run all tests
test:
	$(GO) test ./... -timeout 60s

## test-verbose: run all tests with verbose output
test-verbose:
	$(GO) test ./... -timeout 60s -v

## test-race: run tests with the race detector
test-race:
	$(GO) test ./... -timeout 60s -race

## tidy: tidy go.mod and go.sum
tidy:
	$(GO) mod tidy

## fmt: format all Go source files
fmt:
	$(GO) fmt ./...

## lint: format and vet all Go source files
lint: fmt
	$(GO) vet ./...

## test-e2e: run Playwright end-to-end tests (builds dicode binary first)
test-e2e:
	npx playwright test

## test-e2e-headed: run e2e tests in headed mode (shows browser)
test-e2e-headed:
	npx playwright test --headed

## test-e2e-ui: open Playwright UI mode
test-e2e-ui:
	npx playwright test --ui

## clean: remove compiled binaries
clean:
	rm -f $(BINARY) $(DAEMON_BINARY)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
