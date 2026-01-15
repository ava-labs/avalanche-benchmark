`make pack` to pack a distribution into `avalanche-benchmark.tar.gz`

Update gas targets in `chain-config.json` in both fields - hex and decimal.

Current `gasLimit` is 200m and `min-block-gas-cost` is 1500 - good for immediate results.
I recommend `gasLimit` set to 60m and `min-block-gas-cost` to 560 for ~200m gas per second (~9500 TPS). 

Going from delay of 2000ms to 100ms takes ~3 hours of block production.

On the airgapped machine:
```bash
tar -xzf avalanche-benchmark.tar.gz
./bin/benchmark
```

In another terminal:
```bash
./bin/bombard -batch 500 -keys 500
```
