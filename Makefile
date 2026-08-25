# Conflux — LLM gateway
#
# Standard Go project Makefile. Targets are self-documenting via the `##` marker
# and surfaced by `make help` (the default target). Optional tools (gofumpt,
# golangci-lint, govulncheck) are detected and degrade to stdlib equivalents
# when absent.
#
# Usage:
#   make              # show available targets
#   make build && make run
#   make test         # run the test suite

# ---- Configuration ----------------------------------------------------------

BINARY   := conflux
PKG      := github.com/not-lucky/conflux
CMD_DIR  := ./cmd/conflux
CONFIG   ?= config.yaml
GOFLAGS  :=
GO       := go

# Optional tooling. Resolved lazily inside each target so a missing binary
# degrades gracefully instead of breaking unrelated targets.
GOFUMPT       := $(shell command -v gofumpt 2>/dev/null)
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null)
GOVULNCHECK   := $(shell command -v govulncheck 2>/dev/null)

# ---- Release build ----------------------------------------------------------
# `make release` builds a stripped, statically-linked, optimized binary with
# trimmed paths and a version stamp derived from git. Cross-compile with
# GOOS=/GOARCH=, e.g. `make release GOOS=linux GOARCH=arm64`.
BASE_VERSION := 3.0
GIT_INFO     := $(shell git describe --tags --always --dirty 2>/dev/null)
ifeq ($(GIT_INFO),)
RELEASE_VERSION := $(BASE_VERSION)
else
RELEASE_VERSION := $(BASE_VERSION)-$(GIT_INFO)
endif
# Strip DWARF + symbol table (-s -w), drop build id, and stamp the version.
RELEASE_LDFLAGS := -s -w -buildid= -X $(PKG)/internal/version.Version=$(RELEASE_VERSION)
# Output name: plain `conflux` for host builds; suffixed when cross-compiling.
HOST_OS    := $(shell go env GOOS)
HOST_ARCH  := $(shell go env GOARCH)
ifeq ($(GOOS)$(GOARCH),)
RELEASE_BIN := dist/$(BINARY)
else
RELEASE_BIN := dist/$(BINARY)-$(or $(GOOS),$(HOST_OS))-$(or $(GOARCH),$(HOST_ARCH))
endif

# Allow CONFIG_PATH or an explicit CONFIG=... to drive `make run`.
ifdef CONFIG_PATH
CONFIG := $(CONFIG_PATH)
endif

# ---- Default ----------------------------------------------------------------

.DEFAULT_GOAL := help

# ---- Targets ----------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { \
		printf "\nConflux — targets:\n\n"; \
		fmt = "  \033[36m%-18s\033[0m %s\n"; \
	} \
	/^\.PHONY:/ { next } \
	/^[a-zA-Z0-9_-]+:.*##/ { \
		name = $$1; sub(/:.*/, "", name); \
		desc = $$0; sub(/^[^#]*##[ ]*/, "", desc); \
		printf fmt, name, desc; \
	} \
	END { printf "\nOptional tools: gofumpt=%s golangci-lint=%s govulncheck=%s\n", \
		"$(GOFUMPT)", "$(GOLANGCI_LINT)", "$(GOVULNCHECK)"; }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the binary into ./conflux
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD_DIR)

.PHONY: run
run: build ## Build and run with config.yaml (override with CONFIG=path or CONFIG_PATH)
	@echo ">> running $(BINARY) with config $(CONFIG)"
	./$(BINARY) -config $(CONFIG)

.PHONY: test
test: ## Run the test suite
	$(GO) test $(GOFLAGS) ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race $(GOFLAGS) ./...

.PHONY: test-verbose
test-verbose: ## Run tests verbosely (-v)
	$(GO) test -v $(GOFLAGS) ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format sources (go fmt; gofumpt -w if installed)
	$(GO) fmt ./...
ifdef GOFUMPT
	$(GOFUMPT) -w .
endif

.PHONY: fmt-check
fmt-check: ## Fail if any file is not formatted
	@out=$$(gofmt -l . 2>&1); \
	if [ -n "$$out" ]; then \
		echo "files need formatting (gofmt):"; echo "$$out"; exit 1; \
	fi
ifdef GOFUMPT
	@diff=$$($(GOFUMPT) -d . 2>&1); \
	if [ -n "$$diff" ]; then \
		echo "gofumpt would change files:"; echo "$$diff"; exit 1; \
	fi
endif

.PHONY: lint
lint: ## Lint (golangci-lint if installed, else go vet)
ifdef GOLANGCI_LINT
	$(GOLANGCI_LINT) run
else
	@echo ">> golangci-lint not found; falling back to go vet"
	$(GO) vet ./...
endif

.PHONY: cover
cover: ## Run tests with coverage and open the HTML report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out
	@rm -f coverage.out

.PHONY: cover-summary
cover-summary: ## Print a coverage summary (no browser)
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -bench=. -benchmem ./...

.PHONY: vuln
vuln: ## Run govulncheck (if installed)
ifdef GOVULNCHECK
	$(GOVULNCHECK) ./...
else
	@echo ">> govulncheck not installed; install with:"
	@echo "   $(GO) install golang.org/x/vuln/cmd/govulncheck@latest"
endif

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: install
install: ## Install the binary into GOBIN/GOPATH/bin
	$(GO) install $(CMD_DIR)

.PHONY: release
release: ## Build a stripped, static, optimized release binary into dist/ (stamped version)
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -buildvcs=false -ldflags="$(RELEASE_LDFLAGS)" -o $(RELEASE_BIN) $(CMD_DIR)
	@echo ">> built $(RELEASE_BIN)  (version $(RELEASE_VERSION))"
	@ls -lh $(RELEASE_BIN) | awk '{print "   size : " $$5}'
	@if command -v file >/dev/null 2>&1; then echo "   type :$$(file $(RELEASE_BIN) | cut -d: -f2-)"; fi
	@if command -v ldd >/dev/null 2>&1; then ldd $(RELEASE_BIN) >/dev/null 2>&1 && echo "   links: dynamic" || echo "   links: static (not a dynamic executable)"; fi
	@echo ">> flags: CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags='-s -w -buildid= -X ...version.Version=$(RELEASE_VERSION)'"

.PHONY: check
check: vet test ## Run vet + tests (the pre-push gate)

.PHONY: ci
ci: fmt-check vet test-race ## Run the full local CI gate (fmt-check + vet + race tests)
	@echo ">> ci green"

.PHONY: clean
clean: ## Remove built artifacts
	rm -f $(BINARY) $(BINARY).exe coverage.out
	rm -rf dist
	@find . -type f -name '*.test' -delete 2>/dev/null || true
	@find . -type f -name '*.out' -delete 2>/dev/null || true

.PHONY: config-check
config-check: build ## Validate the config by starting the server briefly
	@echo ">> config-check: load and exit on $(CONFIG)"
	@timeout 1s ./$(BINARY) -config $(CONFIG) 2>&1 || true
