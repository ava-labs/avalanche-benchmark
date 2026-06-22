.PHONY: all clean deps build pack monitoring-deps

.DEFAULT_GOAL := all

AVALANCHEGO_REPO=https://github.com/ava-labs/avalanchego.git
AVALANCHEGO_REF=configure-genesis-acp226-excess-50ms-window
AVALANCHEGO_COMMIT=8497956cbc0851fab40bb7a587d3dd855b7bc770
AVALANCHEGO_BUILD_DIR=/tmp/avalanchego-build-$(AVALANCHEGO_COMMIT)

SUBNET_EVM_ID=srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

# Monitoring stack — run on the control host (linux-amd64 binaries)
PROMETHEUS_VERSION=2.54.1
GRAFANA_VERSION=11.2.2
PROMETHEUS_BASE_URL=https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)
GRAFANA_BASE_URL=https://dl.grafana.com/oss/release

all: deps build
	@echo "All ready."

# Build Go tools
build: bin/create-l1 bin/bombard bin/reconcile

bin/create-l1:
	@mkdir -p bin
	go build -o bin/create-l1 ./cmd/create-l1

bin/bombard:
	@mkdir -p bin
	go build -o bin/bombard ./cmd/bombard

bin/reconcile:
	@mkdir -p bin
	go build -o bin/reconcile ./cmd/reconcile

# Download/build dependencies
deps: bin/avalanchego
	@echo "Dependencies ready."

clean:
	rm -rf bin/

# Build avalanchego + subnet-evm from the published pinned ref (run on Linux)
bin/avalanchego bin/$(SUBNET_EVM_ID):
	@mkdir -p bin
	rm -rf $(AVALANCHEGO_BUILD_DIR)
	git clone --depth 1 --branch $(AVALANCHEGO_REF) $(AVALANCHEGO_REPO) $(AVALANCHEGO_BUILD_DIR)
	cd $(AVALANCHEGO_BUILD_DIR) && test "$$(git rev-parse HEAD)" = "$(AVALANCHEGO_COMMIT)"
	cd $(AVALANCHEGO_BUILD_DIR) && ./scripts/build.sh
	cd $(AVALANCHEGO_BUILD_DIR) && ./graft/subnet-evm/scripts/build.sh || true
	cp $(AVALANCHEGO_BUILD_DIR)/build/avalanchego bin/avalanchego
	cp $(AVALANCHEGO_BUILD_DIR)/build/subnet-evm bin/$(SUBNET_EVM_ID)

# Monitoring binaries (linux-amd64) for the control host: `make monitoring-deps`,
# then `./04_monitoring.sh`. Kept out of `deps` so a normal build doesn't pull
# ~100MB of grafana; make's file targets make it a no-op once fetched.
monitoring-deps: bin/prometheus bin/grafana.tar.gz
	@echo "Monitoring binaries ready (run ./04_monitoring.sh)."

bin/prometheus:
	@mkdir -p bin
	@URL="$(PROMETHEUS_BASE_URL)/prometheus-$(PROMETHEUS_VERSION).linux-amd64.tar.gz"; \
	echo "Downloading prometheus from $$URL..."; \
	curl -L -o /tmp/prometheus.tar.gz "$$URL"; \
	tar -xzf /tmp/prometheus.tar.gz -C /tmp; \
	cp /tmp/prometheus-$(PROMETHEUS_VERSION).linux-amd64/prometheus bin/prometheus; \
	chmod +x bin/prometheus; \
	rm -rf /tmp/prometheus.tar.gz /tmp/prometheus-$(PROMETHEUS_VERSION).linux-amd64

bin/grafana.tar.gz:
	@mkdir -p bin
	@URL="$(GRAFANA_BASE_URL)/grafana-$(GRAFANA_VERSION).linux-amd64.tar.gz"; \
	echo "Downloading grafana from $$URL..."; \
	curl -L -o bin/grafana.tar.gz "$$URL"

# Create offline package for deployment to the control machine
pack: deps build monitoring-deps
	rm -f remote-benchmark.tar.gz
	tar --exclude=scripts/failover/intentions.json --exclude=bin/grafana-dist -czvf remote-benchmark.tar.gz \
		bin/ \
		_common.sh \
		0[1-6]_*.sh \
		scripts/failover/ \
		monitoring/grafana-datasources.yml \
		monitoring/dashboards/ \
		node-config.json \
		chain-config.json \
		chain-config-rpc.json \
		subnet-config.json \
		genesis.json \
		staking/ \
		.env.example \
		README.md
