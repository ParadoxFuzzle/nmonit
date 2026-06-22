#!/usr/bin/make -f

# ============================================================================
# Distributed Compute Fabric — Build System
# ============================================================================

PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

# --- Colors ---
GREEN  := \033[0;32m
YELLOW := \033[0;33m
RED    := \033[0;31m
NC     := \033[0m # No Color

.PHONY: all
all: proto agent control-plane ## Build everything

# ============================================================================
# Protobuf
# ============================================================================

.PHONY: proto
proto: ## Generate code from protobuf definitions
	@echo "$(GREEN)[proto]$(NC) Generating code..."
	cd $(PROJECT_ROOT)/proto && buf generate
	@echo "$(GREEN)[proto]$(NC) Done."

.PHONY: proto-lint
proto-lint: ## Lint protobuf definitions
	cd $(PROJECT_ROOT)/proto && buf lint
	cd $(PROJECT_ROOT)/proto && buf breaking --against '.git#branch=main' || true

# ============================================================================
# Agent (Rust)
# ============================================================================

.PHONY: agent
agent: ## Build the node agent
	@echo "$(GREEN)[agent]$(NC) Building..."
	cargo build --release -p compute-agent
	@echo "$(GREEN)[agent]$(NC) Binary: target/release/compute-agent"

.PHONY: agent-dev
agent-dev: ## Build the node agent (debug)
	cargo build -p compute-agent

.PHONY: agent-test
agent-test: ## Run agent tests
	cargo test -p compute-agent

.PHONY: agent-lint
agent-lint: ## Lint agent code
	cargo clippy -p compute-agent -- -D warnings
	cargo fmt -p compute-agent -- --check

# ============================================================================
# Control Plane (Go)
# ============================================================================

.PHONY: control-plane
control-plane: ## Build the control plane
	@echo "$(GREEN)[control-plane]$(NC) Building..."
	mkdir -p $(PROJECT_ROOT)/bin
	cd $(PROJECT_ROOT)/control-plane && go build -o $(PROJECT_ROOT)/bin/control-plane ./cmd/control-plane
	@echo "$(GREEN)[control-plane]$(NC) Binary: bin/control-plane"

.PHONY: control-plane-dev
control-plane-dev: ## Build the control plane (with race detector)
	mkdir -p $(PROJECT_ROOT)/bin
	cd $(PROJECT_ROOT)/control-plane && go build -race -o $(PROJECT_ROOT)/bin/control-plane ./cmd/control-plane

.PHONY: control-plane-test
control-plane-test: ## Run control plane tests
	cd $(PROJECT_ROOT)/control-plane && go test -race -count=1 ./...

.PHONY: control-plane-lint
control-plane-lint: ## Lint control plane code (go vet + nullable staticcheck + golangci-lint)
	cd $(PROJECT_ROOT)/control-plane && go vet ./...
	cd $(PROJECT_ROOT)/control-plane && staticcheck ./... 2>/dev/null || echo "staticcheck not installed, skipping"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "$(GREEN)[lint]$(NC) Running golangci-lint on control-plane..."; \
		cd $(PROJECT_ROOT)/control-plane && golangci-lint run ./...; \
	else \
		echo "$(YELLOW)[lint] golangci-lint not installed, skipping. Hint: install v1.50+ (https://golangci-lint.run)$(NC)"; \
	fi

# ============================================================================
# CLI (Go)
# ============================================================================

.PHONY: cli
cli: ## Build the CLI tool
	@echo "$(GREEN)[cli]$(NC) Building..."
	mkdir -p $(PROJECT_ROOT)/bin
	cd $(PROJECT_ROOT)/cli && go build -o $(PROJECT_ROOT)/bin/compute ./...
	@echo "$(GREEN)[cli]$(NC) Binary: bin/compute"

# ============================================================================
# Testing
# ============================================================================

.PHONY: test
test: agent-test control-plane-test ## Run all tests

.PHONY: check-dead-symbols
check-dead-symbols: ## Scan the repo for re-referenced removed symbols (catalog: scripts/dead-symbols.json)
	@echo "$(GREEN)[lint]$(NC) Checking for dead symbols..."
	@./scripts/check-dead-symbols.sh

.PHONY: lint
lint: proto-lint agent-lint control-plane-lint check-dead-symbols ## Run all linters

# ============================================================================
# Docker
# ============================================================================

.PHONY: docker-agent
docker-agent: ## Build agent Docker image
	docker build -t compute-agent:latest -f deploy/docker/Dockerfile.agent .

.PHONY: docker-control-plane
docker-control-plane: ## Build control plane Docker image
	docker build -t compute-control-plane:latest -f deploy/docker/Dockerfile.control-plane .

# ============================================================================
# Development
# ============================================================================

.PHONY: dev
dev: proto agent-dev control-plane-dev ## Fast development build

.PHONY: watch-agent
watch-agent: ## Watch agent for changes and rebuild
	cargo watch -x 'build -p compute-agent'

.PHONY: watch-control-plane
watch-control-plane: ## Watch control plane for changes and rebuild
	cd $(PROJECT_ROOT)/control-plane && reflex -r '\.go$$' -- go build -o $(PROJECT_ROOT)/bin/control-plane ./cmd/control-plane

.PHONY: clean
clean: ## Clean all build artifacts
	cargo clean
	rm -rf $(PROJECT_ROOT)/bin
	rm -rf $(PROJECT_ROOT)/control-plane/gen

# ============================================================================
# Help
# ============================================================================

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "$(GREEN)%-24s$(NC) %s\n", $$1, $$2}'
