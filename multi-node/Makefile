.PHONY: all clean deps build pack

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH=configure-genesis-acp226-excess
PROMETHEUS_VERSION=2.54.1
GRAFANA_VERSION=11.2.2
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

# Download/build dependencies
deps: bin/avalanchego bin/prometheus bin/grafana.tar.gz
	@echo "Dependencies ready."

clean:
	rm -rf bin/

# Build avalanchego + subnet-evm from source (run on Linux)
bin/avalanchego bin/$(SUBNET_EVM_ID):
	@mkdir -p bin
	rm -rf /tmp/avalanchego-build
	git clone --depth 1 --branch $(AVALANCHEGO_BRANCH) https://github.com/ava-labs/avalanchego.git /tmp/avalanchego-build
	cd /tmp/avalanchego-build && ./scripts/build.sh
	cd /tmp/avalanchego-build && ./graft/subnet-evm/scripts/build.sh || true
	cp /tmp/avalanchego-build/build/avalanchego bin/avalanchego
	cp /tmp/avalanchego-build/build/subnet-evm bin/$(SUBNET_EVM_ID)
	rm -rf /tmp/avalanchego-build

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

# Download grafana (linux-amd64 for remote nodes) - keep as tarball for fast copying
bin/grafana.tar.gz:
	@mkdir -p bin
	@URL="$(GRAFANA_BASE_URL)/grafana-$(GRAFANA_VERSION).linux-amd64.tar.gz"; \
	echo "Downloading grafana from $$URL..."; \
	curl -L -o bin/grafana.tar.gz "$$URL"

# Create offline package for deployment to another machine
pack: deps build
	rm -f avalanche-benchmark-multi.tar.gz
	tar -czvf avalanche-benchmark-multi.tar.gz \
		bin/ \
		_common.sh \
		0[1-6]_*.sh \
		start-grafana.sh \
		node-config.json \
		chain-config.json \
		genesis.json \
		grafana-datasources.yml \
		avalanche-dashboard.json \
		.env.example \
		README.md
