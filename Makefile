BINARY  := dicode
CMD     := ./cmd/dicode
VERSION ?= dev

GO      := $(shell which go 2>/dev/null || echo $(HOME)/.local/share/mise/shims/go)
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-verbose test-race lint fmt clean run tidy help

## build: compile the dicode binary
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)

## run: build and run the daemon (Ctrl-C to stop)
run: build
	./$(BINARY) daemon

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

## clean: remove compiled binary
clean:
	rm -f $(BINARY)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
