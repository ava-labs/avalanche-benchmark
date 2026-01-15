#!/bin/bash
set -e

echo "================================================"
echo "  Avalanche Benchmark - Airtight Setup"
echo "================================================"
echo ""

# Get the script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCHMARK_DIR="$(dirname "$SCRIPT_DIR")"

# 1. Setup AvalancheGo and plugins
echo "[1/3] Installing AvalancheGo and subnet-evm plugin..."
bash "$SCRIPT_DIR/setup-avalanchego.sh"
echo ""

# 2. Setup environment variables
echo "[2/3] Configuring environment..."
source "$SCRIPT_DIR/setup-env.sh"
echo ""

# 3. Make binaries executable
echo "[3/3] Setting up benchmark CLI and bombard..."

# Setup benchmark binary
if [ -f "$BENCHMARK_DIR/benchmark" ]; then
    chmod +x "$BENCHMARK_DIR/benchmark"
    echo "✓ Benchmark CLI ready at: $BENCHMARK_DIR/benchmark"
elif [ -f "$BENCHMARK_DIR/benchmark-linux" ]; then
    chmod +x "$BENCHMARK_DIR/benchmark-linux"
    mv "$BENCHMARK_DIR/benchmark-linux" "$BENCHMARK_DIR/benchmark"
    echo "✓ Benchmark CLI ready at: $BENCHMARK_DIR/benchmark"
else
    echo "⚠ Warning: benchmark binary not found"
fi

# Setup bombard binary
if [ -f "$BENCHMARK_DIR/bombard" ]; then
    chmod +x "$BENCHMARK_DIR/bombard"
    echo "✓ Bombard binary ready at: $BENCHMARK_DIR/bombard"
elif [ -f "$BENCHMARK_DIR/bombard-linux" ]; then
    chmod +x "$BENCHMARK_DIR/bombard-linux"
    mv "$BENCHMARK_DIR/bombard-linux" "$BENCHMARK_DIR/bombard"
    echo "✓ Bombard binary ready at: $BENCHMARK_DIR/bombard"
else
    echo "⚠ Warning: bombard binary not found"
fi

# 4. Verify staking keys
if [ -d "$BENCHMARK_DIR/staking/local" ]; then
    KEY_COUNT=$(ls -1 "$BENCHMARK_DIR/staking/local/staker*.crt" 2>/dev/null | wc -l)
    echo "✓ Found $KEY_COUNT staking key sets"
else
    echo "⚠ Warning: staking keys not found at $BENCHMARK_DIR/staking/local"
fi

echo ""
echo "================================================"
echo "  Setup Complete!"
echo "================================================"
echo ""
echo "Next steps:"
echo "  1. source ~/.benchmark-env"
echo "  2. cd $BENCHMARK_DIR"
echo "  3. ./benchmark start --l1-validators 5 --l1-rpcs 2"
echo ""
