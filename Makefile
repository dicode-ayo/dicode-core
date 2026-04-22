BINARY  := dicode
CMD     := ./cmd/dicode
VERSION ?= dev

GO      := $(shell which go 2>/dev/null || echo $(HOME)/.local/share/mise/shims/go)
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-verbose test-race lint fmt format format-check clean run tidy help \
	test-e2e test-e2e-unauth test-e2e-auth test-e2e-headed test-e2e-ui test-e2e-install \
	test-tasks test-e2e-relay

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

## format: alias for fmt
format: fmt

## format-check: fail if any Go source file needs formatting (matches CI)
format-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files need formatting (run 'make format'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

## lint: format and vet all Go source files
lint: fmt
	$(GO) vet ./...

E2E_WEBHOOK_SECRET ?= e2e-test-webhook-secret-xyz

## test-e2e-install: install npm deps + Playwright Chromium (one-time)
test-e2e-install:
	npm install
	npx playwright install chromium

## test-e2e-unauth: run unauthenticated + webui Playwright projects (~3 min)
test-e2e-unauth:
	TEST_WEBHOOK_SECRET=$(E2E_WEBHOOK_SECRET) \
		npx playwright test --project=unauthenticated --project=webui

## test-e2e-auth: run authenticated Playwright project (~20 s)
test-e2e-auth:
	DICODE_AUTH_MODE=authenticated \
		npx playwright test --project=authenticated

## test-e2e: full Playwright suite — unauth + webui + auth (~3.5 min)
test-e2e: test-e2e-unauth test-e2e-auth

## test-e2e-headed: run e2e tests in headed mode (shows browser)
test-e2e-headed:
	TEST_WEBHOOK_SECRET=$(E2E_WEBHOOK_SECRET) \
		npx playwright test --project=unauthenticated --project=webui --headed

## test-e2e-ui: open Playwright UI mode
test-e2e-ui:
	TEST_WEBHOOK_SECRET=$(E2E_WEBHOOK_SECRET) \
		npx playwright test --ui

# Deno binary dicode uses internally — installed under ~/.cache/dicode/deno/
# by the managed-runtime bootstrap. Fall back to $PATH.
DENO := $(shell ls -1t $(HOME)/.cache/dicode/deno/*/deno 2>/dev/null | head -1 || which deno 2>/dev/null)

## test-e2e-relay: run Go + testcontainers e2e suite against a real dicode-relay image (requires Docker)
test-e2e-relay:
	@command -v docker >/dev/null 2>&1 || { echo "docker binary not found — install Docker Desktop or Engine first"; exit 1; }
	$(GO) test -tags e2e -timeout 180s ./tests/e2e/relay/...

## test-tasks: run Deno unit tests for tasks/buildin/*/task.test.ts
test-tasks:
	@test -n "$(DENO)" || { echo "deno not found — install or run dicode daemon once to bootstrap"; exit 1; }
	$(DENO) test --allow-all --config=tasks/deno.json 'tasks/buildin/**/task.test.ts'

## clean: remove compiled binary
clean:
	rm -f $(BINARY)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
