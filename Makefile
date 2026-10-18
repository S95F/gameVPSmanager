# gameVPSmanager build.
#
# Development happens on Windows; the only deployment target is a Debian 12 /
# Ubuntu 22.04-24.04 VPS. `make build` therefore cross-compiles for Linux by
# default -- a native Windows binary is only ever useful for running tests.

BINARY      := gvm
MODULE      := github.com/S95F/gameVPSmanager
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# CGO is disabled deliberately. It keeps the binary fully static, so it runs on
# any glibc/musl VPS regardless of the build host's libc version, and it rules
# out cgo-dependent database drivers -- pgx in pure-Go mode is the intended
# Postgres path anyway.
export CGO_ENABLED := 0

LDFLAGS := -s -w \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.Commit=$(COMMIT)'

GO      ?= go
BIN_DIR := bin

.DEFAULT_GOAL := build

.PHONY: build
build: ## Cross-compile the Linux amd64 binary (the deploy target)
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

.PHONY: build-arm64
build-arm64: ## Cross-compile for arm64 VPSes (Ampere, Graviton, Oracle free tier)
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-arm64 ./cmd/$(BINARY)

.PHONY: build-native
build-native: ## Build for the host OS (local experimentation only)
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ ./cmd/...

# -race needs cgo. Only the test binaries get it; release builds keep the
# exported CGO_ENABLED=0 and stay static.
.PHONY: test
test: ## Run the full test suite with the race detector
	CGO_ENABLED=1 $(GO) test ./... -count=1 -race

.PHONY: test-short
test-short: ## Run tests without the race detector (faster inner loop)
	$(GO) test ./... -count=1

.PHONY: cover
cover: ## Write and summarise a coverage profile
	$(GO) test ./... -count=1 -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Rewrite source with gofmt
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted (CI gate)
	@unformatted=$$(gofmt -l . | grep -v '^legacy/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: docs
docs: ## Check user docs stay in sync with the build (hints resolve, README table, CLI reference)
	$(GO) test ./internal/cli -run 'TestHintsResolve|TestReadmeUnbuiltMatchesRegistry|TestAvailabilityMatchesBehaviour|TestCLIReferenceIsUpToDate' -count=1

.PHONY: docs-cli
docs-cli: ## Regenerate docs/cli-reference.md from the dispatch tables
	$(GO) run ./tools/docscli

.PHONY: shellcheck
shellcheck: ## Lint the vendored bash suite, if shellcheck is installed
	@command -v shellcheck >/dev/null 2>&1 \
		&& shellcheck legacy/projectzomboid-bash/*.sh legacy/projectzomboid-bash/lib/*.sh \
		|| echo "shellcheck not installed, skipping"

.PHONY: check
check: fmt-check vet test ## Everything CI runs

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
