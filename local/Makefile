.PHONY: all clean pack pack-blockscout kill run build

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH=configure-genesis-acp226-excess
SUBNET_EVM_ID=srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
BLOCKSCOUT_BUNDLE=../blockscout/images.tar.gz

all: bin/startnetwork bin/bombard bin/avalanchego plugins/$(SUBNET_EVM_ID)

clean:
	rm -rf bin/ plugins/ network_data/

kill:
	-pkill -9 -f avalanchego
	-pkill -9 -f $(SUBNET_EVM_ID)
	rm -rf network_data/

run: kill bin/startnetwork bin/avalanchego plugins/$(SUBNET_EVM_ID)
	./bin/startnetwork

build:
	make clean
	make bin/startnetwork bin/bombard

# Minimal offline tarball: avalanchego + subnet-evm + bombard + benchmark configs.
# Extracts flat: ./bin/, ./plugins/, ./*.json, ./blockscout.sh, etc.
pack: build bin/avalanchego plugins/$(SUBNET_EVM_ID)
	rm -f local-benchmark.tar.gz
	tar -czvf local-benchmark.tar.gz \
		finality-bench-v2.js \
		benchmark-config.json \
		chain-config.json \
		genesis.json \
		node-config.json \
		bin/ \
		plugins/ \
		staking/ \
		blockscout.sh \
		README.md

# Same tarball plus the optional Blockscout explorer (compose stack + saved
# OCI images + helper scripts). Targets podman by default on the operator
# machine. The bundle is loaded on first run via
# `podman load -i blockscout/images.tar.gz`.
pack-blockscout: build bin/avalanchego plugins/$(SUBNET_EVM_ID) $(BLOCKSCOUT_BUNDLE)
	rm -f local-benchmark-with-blockscout.tar.gz
	tar -czvf $(CURDIR)/local-benchmark-with-blockscout.tar.gz \
		finality-bench-v2.js \
		benchmark-config.json \
		chain-config.json \
		genesis.json \
		node-config.json \
		bin/ \
		plugins/ \
		staking/ \
		blockscout.sh \
		README.md \
		-C .. \
		blockscout/docker-compose.yml \
		blockscout/images.conf \
		blockscout/images.tar.gz \
		blockscout/README.md \
		scripts/_blockscout_runtime.sh \
		scripts/blockscout-up.sh \
		scripts/blockscout-down.sh \
		scripts/blockscout-smoke.sh

# Build the offline image archive on a machine with internet. Skipped if the
# archive already exists; remove the file to force a rebuild.
$(BLOCKSCOUT_BUNDLE):
	../scripts/blockscout-pack-images.sh $(CURDIR)/$(BLOCKSCOUT_BUNDLE)

bin/startnetwork:
	go build -o bin/startnetwork ./cmd/startnetwork

bin/bombard:
	go build -o bin/bombard ./cmd/bombard

bin/avalanchego plugins/$(SUBNET_EVM_ID):
	@mkdir -p bin plugins
	rm -rf /tmp/avalanchego-build
	git clone --depth 1 --branch $(AVALANCHEGO_BRANCH) https://github.com/ava-labs/avalanchego.git /tmp/avalanchego-build
	cd /tmp/avalanchego-build && ./scripts/build.sh
	cd /tmp/avalanchego-build && ./graft/subnet-evm/scripts/build.sh || true
	cp /tmp/avalanchego-build/build/avalanchego bin/avalanchego
	cp /tmp/avalanchego-build/build/subnet-evm plugins/$(SUBNET_EVM_ID)
	rm -rf /tmp/avalanchego-build
