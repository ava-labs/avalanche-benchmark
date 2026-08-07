.PHONY: all clean build pack pack-fast package-build bin/l1 bin/fleet bin/oracle bin/bombard bin/VERSIONS

.DEFAULT_GOAL := all

export GOFLAGS := -trimpath

GO_TOOLCHAIN := go1.26.5
PACKAGE_GO_ENV := GOTOOLCHAIN=$(GO_TOOLCHAIN) \
	GOPATH=/tmp/avalanche-benchmark-gopath \
	GOMODCACHE=/tmp/avalanche-benchmark-gomodcache \
	GOCACHE=/tmp/avalanche-benchmark-gocache
AVALANCHEGO_REPO := https://github.com/ava-labs/avalanchego.git
AVALANCHEGO_COMMIT := 80c123c996d7dbdab5f2800ed894348df7e11c21
AVALANCHEGO_BUILD_DIR := $(CURDIR)/.build/avalanchego-$(AVALANCHEGO_COMMIT)
SUBNET_EVM_ID := srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

all: build

build: bin/l1 bin/fleet bin/oracle bin/bombard

bin/l1:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/l1 ./cmd/l1

bin/fleet:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/fleet ./cmd/fleet

bin/oracle:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/oracle ./apps/settlement-feed/cmd/oracle

bin/bombard:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/bombard ./cmd/bombard

bin/avalanchego:
	rm -rf $(AVALANCHEGO_BUILD_DIR)
	mkdir -p $(AVALANCHEGO_BUILD_DIR)
	git -C $(AVALANCHEGO_BUILD_DIR) init
	git -C $(AVALANCHEGO_BUILD_DIR) remote add origin $(AVALANCHEGO_REPO)
	git -C $(AVALANCHEGO_BUILD_DIR) fetch --depth=1 origin $(AVALANCHEGO_COMMIT)
	git -C $(AVALANCHEGO_BUILD_DIR) checkout --detach FETCH_HEAD
	test "$$(git -C $(AVALANCHEGO_BUILD_DIR) rev-parse HEAD)" = "$(AVALANCHEGO_COMMIT)"
	cd $(AVALANCHEGO_BUILD_DIR) && $(PACKAGE_GO_ENV) ./scripts/build.sh
	cd $(AVALANCHEGO_BUILD_DIR)/graft/subnet-evm && $(PACKAGE_GO_ENV) go build \
		-ldflags "-X github.com/ava-labs/avalanchego/version.GitCommit=$(AVALANCHEGO_COMMIT)" \
		-o $(AVALANCHEGO_BUILD_DIR)/build/subnet-evm plugin/*.go
	mkdir -p bin
	cp $(AVALANCHEGO_BUILD_DIR)/build/avalanchego bin/avalanchego
	cp $(AVALANCHEGO_BUILD_DIR)/build/subnet-evm bin/$(SUBNET_EVM_ID)

bin/$(SUBNET_EVM_ID): bin/avalanchego
	test -x bin/$(SUBNET_EVM_ID)

bin/VERSIONS: bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet bin/oracle bin/bombard
	bin/avalanchego --version > bin/VERSIONS
	go version bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet bin/oracle bin/bombard >> bin/VERSIONS
	echo "avalanchego commit $(AVALANCHEGO_COMMIT)" >> bin/VERSIONS
	echo "kit commit $$(git rev-parse HEAD)" >> bin/VERSIONS
	cat bin/VERSIONS

package-build: bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet bin/oracle bin/bombard bin/VERSIONS

PACK_FILES := \
	bin/ \
	README.md \
	.env.example \
	examples/ \
	chains/ \
	docs/ \
	playbooks/ \
	monitoring/prometheus.yml \
	monitoring/alerts.yml \
	monitoring/docker-compose.yml \
	monitoring/grafana-datasources.yml \
	monitoring/grafana-dashboards.yml \
	monitoring/fleet-weight-exporter.py \
	monitoring/dashboards/ \
	apps/settlement-feed/dashboards/ \
	apps/settlement-feed/alerts.yml

pack:
	rm -rf bin
	$(MAKE) package-build
	rm -f avalanche-benchmark.tar.gz
	tar -czf avalanche-benchmark.tar.gz $(PACK_FILES)
	tar -tzf avalanche-benchmark.tar.gz

# pack-fast keeps an already built avalanchego and plugin and rebuilds only the
# kit binaries. Use it while iterating; use pack for anything shipped.
pack-fast:
	test -x bin/avalanchego && test -x bin/$(SUBNET_EVM_ID)
	rm -f bin/l1 bin/fleet bin/oracle bin/bombard bin/VERSIONS
	$(MAKE) package-build
	rm -f avalanche-benchmark.tar.gz
	tar -czf avalanche-benchmark.tar.gz $(PACK_FILES)
	tar -tzf avalanche-benchmark.tar.gz

# Client handover: base + one app + this deployment's identities and frozen
# P-chain, secrets stripped, denylist enforced. Run from a deployment root.
APP ?= settlement-feed
BUNDLE ?= avalanche-l1-bundle.zip
bundle:
	bash tools/bundle.sh $(APP) $(BUNDLE)

clean:
	rm -rf bin .build avalanche-benchmark.tar.gz
