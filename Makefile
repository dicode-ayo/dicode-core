BINARY  := dicode
CMD     := ./cmd/dicode
VERSION ?= dev

GO      := go
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-verbose lint clean run tidy

## build: compile the dicode binary into the project root
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)

## run: build and run dicode (Ctrl-C to stop)
run: build
	./$(BINARY)

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

## lint: run go vet
lint:
	$(GO) vet ./...

## clean: remove the compiled binary
clean:
	rm -f $(BINARY)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
