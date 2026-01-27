.PHONY: all clean deps build

.DEFAULT_GOAL := all

AVALANCHEGO_VERSION=v1.14.1
SUBNET_EVM_VERSION=v0.8.0
PROMETHEUS_VERSION=2.54.1
GRAFANA_VERSION=11.2.2
AVALANCHEGO_BASE_URL=https://github.com/ava-labs/avalanchego/releases/download/$(AVALANCHEGO_VERSION)
SUBNET_EVM_BASE_URL=https://github.com/ava-labs/subnet-evm/releases/download/$(SUBNET_EVM_VERSION)
PROMETHEUS_BASE_URL=https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)
GRAFANA_BASE_URL=https://dl.grafana.com/oss/release

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

# Download avalanchego, subnet-evm, prometheus, grafana
deps: bin/avalanchego bin/$(SUBNET_EVM_ID) bin/prometheus bin/grafana
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

# Download prometheus (linux-amd64 for remote nodes)
bin/prometheus:
	@mkdir -p bin
	@URL="$(PROMETHEUS_BASE_URL)/prometheus-$(PROMETHEUS_VERSION).linux-amd64.tar.gz"; \
	echo "Downloading prometheus from $$URL..."; \
	curl -L -o /tmp/prometheus.tar.gz "$$URL"; \
	tar -xzf /tmp/prometheus.tar.gz -C /tmp; \
	cp /tmp/prometheus-$(PROMETHEUS_VERSION).linux-amd64/prometheus bin/prometheus; \
	chmod +x bin/prometheus; \
	rm -rf /tmp/prometheus.tar.gz /tmp/prometheus-$(PROMETHEUS_VERSION).linux-amd64

# Download grafana (linux-amd64 for remote nodes)
bin/grafana:
	@mkdir -p bin
	@URL="$(GRAFANA_BASE_URL)/grafana-$(GRAFANA_VERSION).linux-amd64.tar.gz"; \
	echo "Downloading grafana from $$URL..."; \
	curl -L -o /tmp/grafana.tar.gz "$$URL"; \
	tar -xzf /tmp/grafana.tar.gz -C bin; \
	mv bin/grafana-v$(GRAFANA_VERSION) bin/grafana-dist; \
	rm -rf /tmp/grafana.tar.gz
