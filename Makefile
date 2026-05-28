.PHONY: all clean deps build pack

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH=configure-genesis-acp226-excess

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
	go build -o bin/bombard ./cmd/bombard

# Download/build dependencies
deps: bin/avalanchego
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

# Create offline package for deployment to another machine
pack: deps build
	rm -f remote-benchmark.tar.gz
	tar -czvf remote-benchmark.tar.gz \
		bin/ \
		_common.sh \
		0[1-3]_*.sh \
		05_benchmark.sh \
		06_cleanup.sh \
		node-config.json \
		chain-config.json \
		genesis.json \
		staking/ \
		.env.example \
		README.md
