.PHONY: build fmt lint test check

# Use the flake's Go toolchain when running outside the development shell.
ifeq ($(origin GO),undefined)
GO := $(if $(shell command -v go),go,nix develop --command go)
endif

# Run the pinned linter with the module's own toolchain (go.mod requires a newer
# Go than prebuilt golangci-lint binaries may support), without adding it to go.mod.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

build:
	$(GO) build -trimpath -o bin/t3fleet ./cmd/t3fleet
fmt:
	$(GOLANGCI_LINT) fmt ./...
lint:
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...
test:
	$(GO) test -race -count=1 ./...
check: lint test
