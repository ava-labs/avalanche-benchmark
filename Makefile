.PHONY: all clean deps build pack

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH=configure-genesis-acp226-excess
# Local worktree used for proposer-window experiments (currently WindowDuration=1000ms)
AVALANCHEGO_SRC=/home/ubuntu/avalanchego-configure-genesis-acp226-excess-50ms-window

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

# Build avalanchego + subnet-evm from the local worktree (run on Linux)
bin/avalanchego bin/$(SUBNET_EVM_ID):
	@mkdir -p bin
	cd $(AVALANCHEGO_SRC) && ./scripts/build.sh
	cd $(AVALANCHEGO_SRC) && ./graft/subnet-evm/scripts/build.sh || true
	cp $(AVALANCHEGO_SRC)/build/avalanchego bin/avalanchego
	cp $(AVALANCHEGO_SRC)/build/subnet-evm bin/$(SUBNET_EVM_ID)

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
