.PHONY: all clean deps build pack

.DEFAULT_GOAL := all

AVALANCHEGO_REPO=https://github.com/ava-labs/avalanchego.git
AVALANCHEGO_REF=configure-genesis-acp226-excess-50ms-window
AVALANCHEGO_COMMIT=4f32c33def921bfea9d048e3fd430d14a1fce9c0
AVALANCHEGO_BUILD_DIR=/tmp/avalanchego-build-$(AVALANCHEGO_COMMIT)

SUBNET_EVM_ID=srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

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

# Create offline package for deployment to another machine
pack: deps build
	rm -f remote-benchmark.tar.gz
	tar --exclude=scripts/failover/intentions.json -czvf remote-benchmark.tar.gz \
		bin/ \
		_common.sh \
		0[1-3]_*.sh \
		05_benchmark.sh \
		06_cleanup.sh \
		scripts/failover/ \
		node-config.json \
		chain-config.json \
		subnet-config.json \
		genesis.json \
		staking/ \
		.env.example \
		README.md
