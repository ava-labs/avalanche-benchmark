.PHONY: all clean clean-tools deps build pack monitoring-deps rpm
# The Go tool binaries are phony too: their file rules have no source
# prerequisites, so without this a stale bin/ silently ships into `pack`
# (bit us 2026-07-06). go's build cache keeps the rebuild near-instant.
.PHONY: bin/create-l1 bin/bombard bin/reconcile bin/genstaking bin/fuji-wallet

.DEFAULT_GOAL := all

# Release version for the RPM (RPM versions cannot contain '-'; use a dotted date).
RELEASE_VERSION ?= $(shell date +%Y.%m.%d)

AVALANCHEGO_REPO=https://github.com/ava-labs/avalanchego.git
AVALANCHEGO_REF=containerman17/fde
AVALANCHEGO_COMMIT=084401863ba97267ea95ac25c4f285f183b0045c
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
build: bin/create-l1 bin/bombard bin/reconcile bin/genstaking bin/fuji-wallet

bin/create-l1:
	@mkdir -p bin
	go build -o bin/create-l1 ./cmd/create-l1

bin/bombard:
	@mkdir -p bin
	go build -o bin/bombard ./cmd/bombard

bin/reconcile:
	@mkdir -p bin
	go build -o bin/reconcile ./cmd/reconcile

bin/genstaking:
	@mkdir -p bin
	go build -o bin/genstaking ./cmd/genstaking

bin/fuji-wallet:
	@mkdir -p bin
	go build -o bin/fuji-wallet ./cmd/fuji-wallet

# Download/build dependencies
deps: bin/avalanchego
	@echo "Dependencies ready."

clean:
	rm -rf bin/

# Remove only the Go tool binaries (keeps the expensive avalanchego/plugin and
# monitoring downloads). pack runs this first so a pack can never ship a stale
# tool even if the .PHONY list above drifts.
clean-tools:
	rm -f bin/create-l1 bin/bombard bin/reconcile bin/genstaking bin/fuji-wallet

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
pack: clean-tools deps build monitoring-deps
	rm -f remote-benchmark.tar.gz
	tar --exclude=scripts/failover/intentions.json --exclude=bin/grafana-dist -czvf remote-benchmark.tar.gz \
		bin/ \
		_common.sh \
		0[0-6]_*.sh \
		scripts/failover/ \
		monitoring/grafana-datasources.yml \
		monitoring/dashboards/ \
		node-config.json \
		chain-config.json \
		chain-config-rpc.json \
		subnet-config.json \
		genesis.json \
		.env.example \
		README.md \
		docs/

# Build the airgap RHEL/RPM. Reuses the exact `pack` payload (single source of
# truth): stage the packed tarball into dist/pkgroot, then nfpm packages that tree
# under /opt/avalanche-benchmark. Requires nfpm (https://nfpm.goreleaser.com).
rpm: pack
	rm -rf dist/pkgroot && mkdir -p dist/pkgroot
	tar -xzf remote-benchmark.tar.gz -C dist/pkgroot
	VERSION=$(RELEASE_VERSION) nfpm pkg --config nfpm.yaml --packager rpm \
		--target avalanche-benchmark-$(RELEASE_VERSION).x86_64.rpm
	@echo "Built avalanche-benchmark-$(RELEASE_VERSION).x86_64.rpm"
