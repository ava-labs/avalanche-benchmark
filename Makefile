.PHONY: build clean test package help

# Binary names
BENCHMARK_BINARY=benchmark
BOMBARD_BINARY=bombard

# Build directories
BUILD_DIR=bin
DIST_DIR=dist

# Go build flags
LDFLAGS=-s -w

help:
	@echo "Avalanche Benchmark CLI"
	@echo ""
	@echo "Usage:"
	@echo "  make build          Build benchmark CLI and bombard"
	@echo "  make clean          Remove build artifacts"
	@echo "  make test           Run tests"
	@echo "  make package        Create distribution package"
	@echo "  make deps           Download and build dependencies"
	@echo ""
	@echo "Environment variables:"
	@echo "  AVALANCHEGO_PATH    Path to avalanchego binary"
	@echo "  AVALANCHEGO_PLUGIN_DIR  Path to plugins directory"

build:
	@echo "Building benchmark CLI and bombard..."
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BENCHMARK_BINARY) ./cmd/benchmark
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BOMBARD_BINARY) ./cmd/bombard
	@echo "Built: $(BUILD_DIR)/$(BENCHMARK_BINARY), $(BUILD_DIR)/$(BOMBARD_BINARY)"

clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR) $(DIST_DIR)

test:
	go test -v ./...

deps:
	@echo "Downloading dependencies..."
	go mod tidy

# Create a distribution package with all required binaries
package: build
	@echo "Creating distribution package..."
	@mkdir -p $(DIST_DIR)/avalanche-benchmark
	@cp $(BUILD_DIR)/$(BENCHMARK_BINARY) $(DIST_DIR)/avalanche-benchmark/
	@cp $(BUILD_DIR)/$(BOMBARD_BINARY) $(DIST_DIR)/avalanche-benchmark/
	@mkdir -p $(DIST_DIR)/avalanche-benchmark/plugins

	@# Copy avalanchego if available
	@if [ -n "$(AVALANCHEGO_PATH)" ] && [ -f "$(AVALANCHEGO_PATH)" ]; then \
		cp $(AVALANCHEGO_PATH) $(DIST_DIR)/avalanche-benchmark/; \
	elif [ -f "../avalanchego/build/avalanchego" ]; then \
		cp ../avalanchego/build/avalanchego $(DIST_DIR)/avalanche-benchmark/; \
	else \
		echo "Warning: avalanchego binary not found. Add it manually."; \
	fi

	@# Copy subnet-evm plugin if available
	@if [ -n "$(AVALANCHEGO_PLUGIN_DIR)" ] && [ -f "$(AVALANCHEGO_PLUGIN_DIR)/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy" ]; then \
		cp $(AVALANCHEGO_PLUGIN_DIR)/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy $(DIST_DIR)/avalanche-benchmark/plugins/; \
	elif [ -f "../avalanchego/build/plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy" ]; then \
		cp ../avalanchego/build/plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy $(DIST_DIR)/avalanche-benchmark/plugins/; \
	else \
		echo "Warning: subnet-evm plugin not found. Add it manually."; \
	fi

	@# Copy README
	@cp README.md $(DIST_DIR)/avalanche-benchmark/ 2>/dev/null || echo "No README.md found"

	@# Create tarball
	@cd $(DIST_DIR) && tar -czf avalanche-benchmark.tar.gz avalanche-benchmark
	@echo "Package created: $(DIST_DIR)/avalanche-benchmark.tar.gz"

# Install to GOPATH/bin
install: build
	@echo "Installing to $(GOPATH)/bin..."
	@cp $(BUILD_DIR)/$(BENCHMARK_BINARY) $(GOPATH)/bin/
	@cp $(BUILD_DIR)/$(BOMBARD_BINARY) $(GOPATH)/bin/

.DEFAULT_GOAL := help
