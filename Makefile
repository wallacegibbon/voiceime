# voiceime - build, test, install.
#
# Every target is non-interactive and idempotent, for the same reason the program
# itself is: a person or an agent should be able to drive this without a GUI and
# without answering questions. `make help` lists what exists.
#
# One build tree, one platform. There used to be a `cross` target for a
# windows/darwin/arm64 matrix and a `dist` target for a release tarball: the
# binaries built, and nothing else did - the other half of this tool is
# ibus-daemon, a systemd user unit and PipeWire - and there was no release to
# package, since `VERSION` comes from `git describe` and nothing is tagged.
# Installing is `make install`, from a checkout, by whoever is deploying.

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

# The unreachable-code check is fetched at a pinned version rather than required
# in go.mod: the newest golang.org/x/tools needs Go 1.26, which would raise this
# project's own minimum toolchain - and drag in unrelated updates - for a check
# that never ships. v0.36.0 asks for Go 1.23.
DEADCODE    ?= golang.org/x/tools/cmd/deadcode@v0.36.0
STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck@2025.1

# A speech file to measure against. `make measure AUDIO=speech.wav` replays it at
# real time and prints latency and repair numbers; see docs/measurements.md.
AUDIO       ?=

# --- build ------------------------------------------------------------------ #

.PHONY: all
all: build ## Build the binary (default)

.PHONY: build
build: ## Build bin/voiceime for this machine
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/voiceime
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

.PHONY: install
install: build ## Install to $(DESTDIR)$(PREFIX)/bin (docs to $(PREFIX)/docs)
	@# scripts/ is deliberately not installed. asr-service.sh is run at deploy time, from the
	@# checkout, next to the audio.cpp build and the checkpoint it is pointed at; an installed
	@# copy of it would be a script with no build and no checkpoint beside it. Nothing here
	@# refers to it either: what the program prints stops at the address.
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN_DIR)/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	@# The manual travels with the binary: a machine that has only the installed copy is the
	@# one where there is no checkout to read these from.
	install -d $(DESTDIR)$(PREFIX)/docs
	install -m 0644 docs/*.md $(DESTDIR)$(PREFIX)/docs/
	@# The settings sample travels with the docs: it is the file a settings file starts
	@# as, and a machine with only the installed binary still has nowhere to copy it from
	@# otherwise. Nothing here writes it into ~/.config - that copy is a person's.
	install -m 0644 config.example.toml $(DESTDIR)$(PREFIX)/docs/
	@# The binary embeds OpenCC's t2s tables (Apache-2.0); the licence and notice
	@# travel with it so a machine that has only the installed binary can still
	@# satisfy the redistribution terms.
	install -d $(DESTDIR)$(PREFIX)/licenses/opencc
	install -m 0644 internal/textnorm/data/LICENSE internal/textnorm/data/NOTICE \
		$(DESTDIR)$(PREFIX)/licenses/opencc/
	@echo "installed $(DESTDIR)$(PREFIX)/bin/$(BINARY), $(DESTDIR)$(PREFIX)/docs/ and the OpenCC licence"

.PHONY: uninstall
uninstall: ## Remove the installed binary, docs and licences
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	rm -f $(DESTDIR)$(PREFIX)/docs/*.md
	rm -f $(DESTDIR)$(PREFIX)/docs/config.example.toml
	rm -f $(DESTDIR)$(PREFIX)/licenses/opencc/LICENSE $(DESTDIR)$(PREFIX)/licenses/opencc/NOTICE
	-rmdir $(DESTDIR)$(PREFIX)/licenses/opencc $(DESTDIR)$(PREFIX)/licenses 2>/dev/null


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

.PHONY: unused
unused: ## Fail if any code is unused - including code only a test could call
	@#
	@# deadcode answers "can main reach this", and treats a test as no caller. This is the
	@# other half: a helper that only a test calls, a parameter nothing varies, a constant
	@# from a design that was replaced. Both are kept because a long-lived project accumulates
	@# them faster than anyone notices, and every one is a thing a reader has to understand.
	@bin=$$(mktemp -d); \
	if ! GOBIN="$$bin" $(GO) install $(STATICCHECK) >/dev/null 2>&1; then \
		echo "$(STATICCHECK) could not be installed; this check needs network access once:"; \
		GOBIN="$$bin" $(GO) install $(STATICCHECK); rm -rf "$$bin"; exit 1; \
	fi; \
	out=$$(mktemp); \
	if ! "$$bin/staticcheck" -checks U1000 ./... > "$$out" 2>&1; then \
		echo "unused code:"; cat "$$out"; rm -rf "$$bin" "$$out"; exit 1; \
	fi; \
	rm -rf "$$bin" "$$out"; \
	echo "unused: nothing unused, in the shipped code or in the tests"

.PHONY: docs-check
docs-check: ## Fail if a relative link or anchor in the documentation does not resolve
	@# Build tooling, like deadcode above: it lives under tools/ rather than cmd/,
	@# so it is not part of the shipped command surface and the unreachable-code
	@# check (cmd/ and internal/ only) never reaches it. Go rather than python3:
	@# a machine that builds this project has the toolchain by definition, while
	@# python3 is not a build prerequisite. Stdlib only, so no network.
	@$(GO) run ./tools/check_docs

.PHONY: sh-check
sh-check: ## Fail if a shell script in this project does not parse
	@# scripts/ holds the deployment that is deliberately not compiled into the program, so
	@# nothing in `go build` ever looks at it. A shell script's syntax errors are not its
	@# only ones - this script carries a comment about a nested ${A:-${B:-c}} expansion that
	@# parsed, and quietly produced an empty path, in dash and not in bash - so it is parsed
	@# with /bin/sh, which is the interpreter its own shebang names.
	@for script in $(shell git ls-files 'scripts/*.sh'); do \
		sh -n "$$script" || exit 1; \
	done
	@echo "sh-check: every script in scripts/ parses with /bin/sh"

.PHONY: check
check: fmt-check vet test deadcode unused docs-check sh-check ## Everything CI should run

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
	$(BIN_DIR)/$(BINARY) capabilities --json

.PHONY: verify
verify: check doctor ## Run the tests and then check the machine

# --- housekeeping ----------------------------------------------------------- #

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR)

.PHONY: help
help: ## List the available targets
	@echo "voiceime $(VERSION)"
	@echo
	@echo "usage: make <target>"
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "variables: BIN_DIR=$(BIN_DIR) PREFIX=$(PREFIX) VERSION=$(VERSION)"
