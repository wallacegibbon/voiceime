# voiceime - build, test, install.
#
# Every target is non-interactive and idempotent, for the same reason the program
# itself is: a person or an agent should be able to drive this without a GUI and
# without answering questions. `make help` lists what exists.

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO          ?= go
BINARY      ?= voiceime
BIN_DIR     ?= bin
# Installs go to ~/.local/bin by default rather than /usr/local/bin: the IBus
# component file and the systemd unit both record the *absolute* path of the
# binary, so it has to live somewhere that outlives a rebuild or a moved checkout
# (both of which have broken a real install here), and a per-user prefix needs no
# root. `make install PREFIX=/usr/local` still works and then needs sudo.
PREFIX      ?= $(HOME)/.local
DESTDIR     ?=
PKG         := github.com/wallacegibbon/voiceime
# A version is stamped into the binary so `voiceime capabilities` can report what
# a user actually has; without it the field would just say "dev" on every build.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS     ?=
LDFLAGS     ?= -s -w -X $(PKG)/internal/cli.Version=$(VERSION)
# The whole project is cgo-free: capture is a subprocess and text delivery is
# D-Bus. Pinned here rather than in the environment so a stray CGO_ENABLED=1
# cannot quietly reintroduce a toolkit dependency.
export CGO_ENABLED := 0
# Targets for the multi-platform roadmap. Kept explicit: each one is something we
# intend to support, not an arbitrary matrix.
PLATFORMS   ?= linux/amd64 linux/arm64 windows/amd64 darwin/arm64

# The unreachable-code check is fetched at a pinned version rather than required
# in go.mod: the newest golang.org/x/tools needs Go 1.26, which would raise this
# project's own minimum toolchain - and drag in unrelated updates - for a check
# that never ships. v0.36.0 asks for Go 1.23.
DEADCODE    ?= golang.org/x/tools/cmd/deadcode@v0.36.0

# A speech file to measure against. `make measure AUDIO=speech.wav` replays it at
# real time and prints latency and repair numbers; see docs/measurements.md.
AUDIO       ?=
CHUNK       ?= 3

# --- build ------------------------------------------------------------------ #

.PHONY: all
all: build ## Build the binary (default)

.PHONY: build
build: ## Build bin/voiceime for this machine
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/voiceime
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

.PHONY: install
install: build ## Install to $(DESTDIR)$(PREFIX)/bin (and docs to $(PREFIX)/docs)
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN_DIR)/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	@# The hotkey unit records the absolute path of docs/AGENT.md beside the binary
	@# (dir(binary)/../docs), so the docs are installed too: without them the
	@# unit's Documentation= is a dead link.
	install -d $(DESTDIR)$(PREFIX)/docs
	install -m 0644 docs/*.md $(DESTDIR)$(PREFIX)/docs/
	@echo "installed $(DESTDIR)$(PREFIX)/bin/$(BINARY) and $(DESTDIR)$(PREFIX)/docs/"

.PHONY: uninstall
uninstall: ## Remove the installed binary and docs
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	rm -f $(DESTDIR)$(PREFIX)/docs/*.md

.PHONY: dist
dist: build ## Package a release tarball under dist/
	@mkdir -p dist
	@name=$(BINARY)-$(VERSION)-linux-amd64; \
	rm -rf dist/$$name; mkdir -p dist/$$name/docs; \
	cp $(BIN_DIR)/$(BINARY) dist/$$name/; \
	cp README.md dist/$$name/; \
	cp docs/*.md dist/$$name/docs/; \
	tar -C dist -czf dist/$$name.tar.gz $$name; \
	rm -rf dist/$$name; \
	echo "dist/$$name.tar.gz"

.PHONY: cross
cross: ## Cross-compile for the platforms in PLATFORMS
	@mkdir -p $(BIN_DIR)
	@for platform in $(PLATFORMS); do \
		export GOOS=$${platform%/*} GOARCH=$${platform#*/}; \
		suffix=$${GOOS}-$${GOARCH}; \
		[ "$$GOOS" = windows ] && suffix=$$suffix.exe; \
		echo "building $(BIN_DIR)/$(BINARY)-$$suffix"; \
		$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/$(BINARY)-$$suffix ./cmd/voiceime || exit 1; \
	done

# --- quality ---------------------------------------------------------------- #

.PHONY: test
test: ## Run the unit tests
	$(GO) test $(GOFLAGS) ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format the source
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: deadcode
deadcode: ## Fail if main cannot reach some code in the shipped packages
	@#
	@# The tool is installed into a throwaway GOBIN and run from there, so the
	@# only thing captured is its own report: a first run also has to download the
	@# module, and "go: downloading ..." on stdout would otherwise read as a
	@# finding. A test is not a caller, which is the point of the check.
	@bin=$$(mktemp -d); \
	if ! GOBIN="$$bin" $(GO) install $(DEADCODE) >/dev/null 2>&1; then \
		echo "$(DEADCODE) could not be installed; this check needs network access once:"; \
		GOBIN="$$bin" $(GO) install $(DEADCODE); rm -rf "$$bin"; exit 1; \
	fi; \
	out=$$(mktemp); \
	if ! "$$bin/deadcode" ./cmd/... ./internal/... > "$$out" 2>&1; then \
		echo "deadcode failed:"; cat "$$out"; rm -rf "$$bin" "$$out"; exit 1; \
	fi; \
	if [ -s "$$out" ]; then \
		echo "unreachable code (a test is not a caller):"; cat "$$out"; rm -rf "$$bin" "$$out"; exit 1; \
	fi; \
	rm -rf "$$bin" "$$out"; \
	echo "deadcode: every function in cmd/ and internal/ is reachable from main"

.PHONY: check
check: fmt-check vet test deadcode ## Everything CI should run

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

# --- running ---------------------------------------------------------------- #

.PHONY: run
run: build ## Run the binary with ARGS="..." (default: help)
	$(BIN_DIR)/$(BINARY) $(ARGS)

.PHONY: doctor
doctor: build ## Check this machine against the built binary
	@# No `|| true`: a failing check is a failing target, which is what makes
	@# `make verify` meaningful and what an automated caller relies on. The
	@# report itself is printed either way.
	$(BIN_DIR)/$(BINARY) doctor

.PHONY: capabilities
capabilities: build ## Print the machine-readable command and config surface
	$(BIN_DIR)/$(BINARY) capabilities --pretty

.PHONY: measure
measure: build ## Replay a file and print latency/repair numbers (AUDIO=file CHUNK=3)
	@if [ -z "$(AUDIO)" ]; then \
		echo "usage: make measure AUDIO=speech.wav [CHUNK=3]"; \
		echo "see docs/measurements.md for what the numbers mean"; \
		exit 2; \
	fi
	$(BIN_DIR)/$(BINARY) stream --file "$(AUDIO)" --realtime --chunk $(CHUNK)

.PHONY: verify
verify: check doctor ## Run the tests and then check the machine

# --- housekeeping ----------------------------------------------------------- #

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) dist

.PHONY: help
help: ## List the available targets
	@echo "voiceime $(VERSION)"
	@echo
	@echo "usage: make <target>"
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "variables: BIN_DIR=$(BIN_DIR) PREFIX=$(PREFIX) VERSION=$(VERSION) PLATFORMS='$(PLATFORMS)'"
