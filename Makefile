VERSION ?= 0.0.1
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE           := github.com/Taurine-Technology/axon-pulse
SHARED_BUILDINFO := github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo
DIST_DIR         ?= bin
MAX_BINARY_BYTES := 20971520
MAX_DESKTOP_BYTES := 26214400
MAX_FRONTEND_JS_BYTES := 204800
TARGETS          := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

LDFLAGS := -s -w \
	-X $(SHARED_BUILDINFO).Product=axon-pulse \
	-X $(SHARED_BUILDINFO).Version=$(VERSION) \
	-X $(SHARED_BUILDINFO).Commit=$(COMMIT) \
	-X $(SHARED_BUILDINFO).Date=$(DATE)
DESKTOP_LDFLAGS := $(subst Product=axon-pulse,Product=axon-pulse-desktop,$(LDFLAGS))

GO_LICENSES_VERSION := v2.0.1
LICENSE_ALLOWLIST   := Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC,MPL-2.0

# Pinned meta-linter, config in .golangci.yml (shared with the Axon switch agent so both
# Go codebases enforce the same practices). Run via `go run` like go-licenses:
# version-pinned through the module proxy, cross-platform, no bootstrap step.
# The build is cached by the Go toolchain after the first run.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: build build-all build-desktop size-check desktop-size-check test lint licenses clean

build:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST_DIR)/axon-pulse ./cmd/axon-pulse

build-all:
	@mkdir -p $(DIST_DIR)
	@set -eu; \
	for target in $(TARGETS); do \
		os=$${target%/*}; \
		arch=$${target#*/}; \
		suffix=""; \
		if [ "$$os" = "windows" ]; then suffix=".exe"; fi; \
		output="$(DIST_DIR)/axon-pulse_$${os}_$${arch}$${suffix}"; \
		echo "building $$output"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build \
			-trimpath -ldflags '$(LDFLAGS)' -o "$$output" ./cmd/axon-pulse; \
	done
	@$(MAKE) --no-print-directory size-check

build-desktop:
	@mkdir -p $(DIST_DIR)
	cd desktop && CGO_ENABLED=1 go build -trimpath -ldflags '$(DESKTOP_LDFLAGS)' \
		-o ../$(DIST_DIR)/axon-pulse-desktop .
	@$(MAKE) --no-print-directory desktop-size-check

desktop-size-check:
	@bytes=$$(wc -c < "$(DIST_DIR)/axon-pulse-desktop" | tr -d ' '); \
	jsbytes=$$(wc -c desktop/frontend/*.js desktop/frontend/*.mjs | awk 'END { print $$1 }'); \
	echo "$(DIST_DIR)/axon-pulse-desktop: $$bytes bytes installed"; \
	echo "desktop frontend JavaScript: $$jsbytes bytes"; \
	[ "$$bytes" -le "$(MAX_DESKTOP_BYTES)" ] || { echo "ERROR: desktop exceeds 25 MiB" >&2; exit 1; }; \
	[ "$$jsbytes" -le "$(MAX_FRONTEND_JS_BYTES)" ] || { echo "ERROR: frontend JS exceeds 200 KiB" >&2; exit 1; }

size-check:
	@set -eu; \
	found=0; \
	for binary in $(DIST_DIR)/axon-pulse_*; do \
		[ -f "$$binary" ] || continue; \
		found=1; \
		bytes=$$(wc -c < "$$binary" | tr -d ' '); \
		echo "$$binary: $$bytes bytes"; \
		if [ "$$bytes" -gt "$(MAX_BINARY_BYTES)" ]; then \
			echo "ERROR: $$binary exceeds $(MAX_BINARY_BYTES) bytes" >&2; \
			exit 1; \
		fi; \
	done; \
	if [ "$$found" -eq 0 ]; then \
		echo "ERROR: no cross-compiled binaries found in $(DIST_DIR)" >&2; \
		exit 1; \
	fi

test:
	go test -race ./...
	cd desktop && go test -race ./...
	cd third_party/axon-contracts && go test -race ./...

lint:
	go vet ./...
	cd desktop && go vet ./...
	cd third_party/axon-contracts && go vet ./...
	@test -z "$$(find cmd internal desktop quality tools third_party -name '*.go' -type f -exec gofmt -l {} +)"
	$(GOLANGCI_LINT) run
	cd desktop && $(GOLANGCI_LINT) run

licenses:
	go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
		--allowed_licenses=$(LICENSE_ALLOWLIST) \
		--ignore=$(MODULE) \
		--ignore=github.com/Taurine-Technology/axon-contracts/gen/go
	cd desktop && go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
		--allowed_licenses=$(LICENSE_ALLOWLIST) \
		--ignore=$(MODULE)/desktop --ignore=$(MODULE) \
		--ignore=github.com/Taurine-Technology/axon-contracts/gen/go

clean:
	rm -r $(DIST_DIR)
