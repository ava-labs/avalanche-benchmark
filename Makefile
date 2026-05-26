.PHONY: all clean

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH = configure-genesis-acp226-excess
SUBNET_EVM_ID      = srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
PACKAGE            = avalanche-benchmark-runtime.tar.gz

all: benchctl avalanchego $(SUBNET_EVM_ID) $(PACKAGE)
	@echo "All ready."

benchctl: go.mod go.sum assets.go cmd/benchctl/*.go staking/pchain/1/signer.key staking/pchain/1/staker.crt staking/pchain/1/staker.key staking/pchain/2/signer.key staking/pchain/2/staker.crt staking/pchain/2/staker.key staking/l1/1/signer.key staking/l1/2/signer.key staking/l1/3/signer.key staking/l1/4/signer.key staking/l1/5/signer.key
	go build -o benchctl ./cmd/benchctl

avalanchego $(SUBNET_EVM_ID):
	rm -rf /tmp/avalanchego-build-benchmark
	git clone --depth 1 --branch $(AVALANCHEGO_BRANCH) https://github.com/ava-labs/avalanchego.git /tmp/avalanchego-build-benchmark
	cd /tmp/avalanchego-build-benchmark && ./scripts/build.sh
	cd /tmp/avalanchego-build-benchmark && ./graft/subnet-evm/scripts/build.sh || true
	cp /tmp/avalanchego-build-benchmark/build/avalanchego avalanchego
	cp /tmp/avalanchego-build-benchmark/build/subnet-evm $(SUBNET_EVM_ID)
	chmod +x avalanchego $(SUBNET_EVM_ID)
	rm -rf /tmp/avalanchego-build-benchmark

$(PACKAGE): benchctl avalanchego $(SUBNET_EVM_ID) config/genesis.json config/chain-config.json config/node-config.json .env.example scripts/00_copy-artifacts.sh scripts/01_start-pchain.sh scripts/02_create-l1.sh scripts/lib.sh
	rm -f $(PACKAGE)
	tar -czf $(PACKAGE) benchctl avalanchego $(SUBNET_EVM_ID) config/genesis.json config/chain-config.json config/node-config.json .env.example scripts/00_copy-artifacts.sh scripts/01_start-pchain.sh scripts/02_create-l1.sh scripts/lib.sh

clean:
	rm -f benchctl avalanchego $(SUBNET_EVM_ID) $(PACKAGE)
	rm -rf runtime-data
