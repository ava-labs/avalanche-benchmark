.PHONY: all clean build pack package-build bin/l1 bin/fleet bin/VERSIONS

.DEFAULT_GOAL := all

export GOFLAGS := -trimpath

GO_TOOLCHAIN := go1.26.5
PACKAGE_GO_ENV := GOTOOLCHAIN=$(GO_TOOLCHAIN) \
	GOPATH=/tmp/avalanche-benchmark-gopath \
	GOMODCACHE=/tmp/avalanche-benchmark-gomodcache \
	GOCACHE=/tmp/avalanche-benchmark-gocache
AVALANCHEGO_REPO := https://github.com/ava-labs/avalanchego.git
AVALANCHEGO_COMMIT := a067df1192c95d4755f76a631ef3c6ed772e977c
AVALANCHEGO_BUILD_DIR := $(CURDIR)/.build/avalanchego-$(AVALANCHEGO_COMMIT)
SUBNET_EVM_ID := srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

all: build

build: bin/l1 bin/fleet

bin/l1:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/l1 ./cmd/l1

bin/fleet:
	mkdir -p bin
	$(PACKAGE_GO_ENV) go build -o bin/fleet ./cmd/fleet

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

bin/VERSIONS: bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet
	bin/avalanchego --version > bin/VERSIONS
	go version bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet >> bin/VERSIONS
	echo "avalanchego commit $(AVALANCHEGO_COMMIT)" >> bin/VERSIONS
	echo "kit commit $$(git rev-parse HEAD)" >> bin/VERSIONS
	cat bin/VERSIONS

package-build: bin/avalanchego bin/$(SUBNET_EVM_ID) bin/l1 bin/fleet bin/VERSIONS

pack:
	rm -rf bin
	$(MAKE) package-build
	rm -f remote-benchmark.tar.gz
	tar -czf remote-benchmark.tar.gz \
		bin/ \
		README.md \
		DESIGN.md \
		.env.example \
		nodes.ini.example \
		genesis-template.json \
		node-config.json \
		chain-config.json \
		chain-config-rpc.json \
		subnet-config.json \
		monitoring/grafana-datasources.yml \
		monitoring/dashboards/
	tar -tzf remote-benchmark.tar.gz

clean:
	rm -rf bin .build remote-benchmark.tar.gz
