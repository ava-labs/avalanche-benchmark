.PHONY: all clean pack kill run build

.DEFAULT_GOAL := all

AVALANCHEGO_BRANCH=configure-genesis-acp226-excess

all: bin/benchmark bin/bombard bin/avalanchego plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy

clean:
	rm -rf bin/ plugins/ network_data/

kill:
	-pkill -9 -f avalanchego
	-pkill -9 -f srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
	rm -rf network_data/

run: kill bin/benchmark bin/avalanchego plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
	./bin/benchmark

build:
	make clean
	make bin/benchmark bin/bombard

pack: build bin/avalanchego plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
	rm -f avalanche-benchmark.tar.gz
	tar -czvf avalanche-benchmark.tar.gz *.json bin/ plugins/ staking/

bin/benchmark:
	go build -o bin/benchmark ./cmd/benchmark

bin/bombard:
	go build -o bin/bombard ./cmd/bombard

bin/avalanchego plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy:
	@mkdir -p bin plugins
	rm -rf /tmp/avalanchego-build
	git clone --depth 1 --branch $(AVALANCHEGO_BRANCH) https://github.com/ava-labs/avalanchego.git /tmp/avalanchego-build
	cd /tmp/avalanchego-build && ./scripts/build.sh
	cd /tmp/avalanchego-build && ./graft/subnet-evm/scripts/build.sh || true
	cp /tmp/avalanchego-build/build/avalanchego bin/avalanchego
	cp /tmp/avalanchego-build/build/subnet-evm plugins/srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy
	rm -rf /tmp/avalanchego-build