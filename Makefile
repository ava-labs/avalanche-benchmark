.PHONY: all clean

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH = configure-genesis-acp226-excess
SUBNET_EVM_ID      = srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
PACKAGE            = avalanche-benchmark-runtime.tar.gz
L1_SIGNER_KEYS     = $(wildcard staking/l1/*/signer.key)

all: create-l1 bombard avalanchego $(SUBNET_EVM_ID) $(PACKAGE)
	@echo "All ready."

create-l1: go.mod go.sum cmd/create-l1/*.go staking/node-ids.env $(L1_SIGNER_KEYS)
	go build -o create-l1 ./cmd/create-l1

bombard: go.mod go.sum cmd/bombard/*.go
	go build -o bombard ./cmd/bombard

avalanchego $(SUBNET_EVM_ID):
	rm -rf /tmp/avalanchego-build-benchmark
	git clone --depth 1 --branch $(AVALANCHEGO_BRANCH) https://github.com/ava-labs/avalanchego.git /tmp/avalanchego-build-benchmark
	cd /tmp/avalanchego-build-benchmark && ./scripts/build.sh
	cd /tmp/avalanchego-build-benchmark && ./graft/subnet-evm/scripts/build.sh || true
	cp /tmp/avalanchego-build-benchmark/build/avalanchego avalanchego
	cp /tmp/avalanchego-build-benchmark/build/subnet-evm $(SUBNET_EVM_ID)
	chmod +x avalanchego $(SUBNET_EVM_ID)
	rm -rf /tmp/avalanchego-build-benchmark

$(PACKAGE): create-l1 bombard avalanchego $(SUBNET_EVM_ID) config/genesis.json config/chain-config.json config/node-config.json config/subnet-config.json .env.example scripts/00_copy-artifacts.sh scripts/01_start-pchain.sh scripts/02_create-l1.sh scripts/03_start-l1.sh scripts/04_bombard.sh scripts/lib.sh staking/node-ids.env
	rm -f $(PACKAGE)
	tar -czf $(PACKAGE) create-l1 bombard avalanchego $(SUBNET_EVM_ID) config .env.example scripts staking

clean:
	rm -f benchctl create-l1 bombard avalanchego $(SUBNET_EVM_ID) $(PACKAGE)
	rm -rf runtime-data
