.PHONY: all clean deps build

.DEFAULT_GOAL := all

AVALANCHEGO_VERSION=v1.14.1
SUBNET_EVM_VERSION=v0.8.0
AVALANCHEGO_BASE_URL=https://github.com/ava-labs/avalanchego/releases/download/$(AVALANCHEGO_VERSION)
SUBNET_EVM_BASE_URL=https://github.com/ava-labs/subnet-evm/releases/download/$(SUBNET_EVM_VERSION)

SUBNET_EVM_ID=srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

all: deps build
	@echo "All ready."

# Build Go tools
build: bin/create-l1 bin/bombard

bin/create-l1:
	@mkdir -p bin
	go build -o bin/create-l1 ./cmd/create-l1

bin/bombard:
	@mkdir -p bin
	cd ../single-node && go build -o ../multi-node/bin/bombard ./cmd/bombard

# Download avalanchego and subnet-evm
deps: bin/avalanchego bin/$(SUBNET_EVM_ID)
	@echo "Dependencies ready."

clean:
	rm -rf bin/

bin/avalanchego:
	@mkdir -p bin
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	ARCH=$$(uname -m); \
	if [ "$$ARCH" = "x86_64" ]; then \
		ARCH_SUFFIX=amd64; \
	elif [ "$$ARCH" = "aarch64" ] || [ "$$ARCH" = "arm64" ]; then \
		ARCH_SUFFIX=arm64; \
	else \
		echo "Unsupported architecture: $$ARCH"; \
		exit 1; \
	fi; \
	if [ "$$OS" = "darwin" ]; then \
		URL="$(AVALANCHEGO_BASE_URL)/avalanchego-macos-$(AVALANCHEGO_VERSION).zip"; \
		echo "Downloading avalanchego from $$URL..."; \
		curl -L -o /tmp/avalanchego.zip "$$URL"; \
		unzip -q /tmp/avalanchego.zip -d /tmp; \
		cp /tmp/avalanchego-$(AVALANCHEGO_VERSION)/avalanchego bin/avalanchego; \
		rm -rf /tmp/avalanchego.zip /tmp/avalanchego-$(AVALANCHEGO_VERSION); \
	else \
		URL="$(AVALANCHEGO_BASE_URL)/avalanchego-linux-$$ARCH_SUFFIX-$(AVALANCHEGO_VERSION).tar.gz"; \
		echo "Downloading avalanchego from $$URL..."; \
		curl -L -o /tmp/avalanchego.tar.gz "$$URL"; \
		tar -xzf /tmp/avalanchego.tar.gz -C bin --strip-components=1 avalanchego-$(AVALANCHEGO_VERSION)/avalanchego; \
		rm /tmp/avalanchego.tar.gz; \
	fi; \
	chmod +x bin/avalanchego

bin/$(SUBNET_EVM_ID):
	@mkdir -p bin
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	ARCH=$$(uname -m); \
	if [ "$$ARCH" = "x86_64" ]; then \
		ARCH_SUFFIX=amd64; \
	elif [ "$$ARCH" = "aarch64" ] || [ "$$ARCH" = "arm64" ]; then \
		ARCH_SUFFIX=arm64; \
	else \
		echo "Unsupported architecture: $$ARCH"; \
		exit 1; \
	fi; \
	if [ "$$OS" = "darwin" ]; then \
		OS_SUFFIX=darwin; \
	else \
		OS_SUFFIX=linux; \
	fi; \
	VERSION_NO_V=$$(echo "$(SUBNET_EVM_VERSION)" | sed 's/^v//'); \
	URL="$(SUBNET_EVM_BASE_URL)/subnet-evm_$${VERSION_NO_V}_$${OS_SUFFIX}_$${ARCH_SUFFIX}.tar.gz"; \
	echo "Downloading subnet-evm from $$URL..."; \
	curl -L -o /tmp/subnet-evm.tar.gz "$$URL"; \
	tar -xzf /tmp/subnet-evm.tar.gz -C /tmp; \
	cp /tmp/subnet-evm bin/$(SUBNET_EVM_ID); \
	chmod +x bin/$(SUBNET_EVM_ID); \
	rm -rf /tmp/subnet-evm.tar.gz /tmp/subnet-evm
