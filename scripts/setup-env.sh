#!/bin/bash
# This script sets up environment variables for the benchmark
# Run with: source ./setup-env.sh

export AVALANCHEGO_PATH="$HOME/.avalanchego/avalanchego"
export AVALANCHEGO_PLUGIN_DIR="$HOME/.avalanchego/plugins"

# Set bombard path if it exists in home directory
if [ -f "$HOME/bombard" ]; then
    export EVMBOMBARD_PATH="$HOME/bombard"
fi

# Add home directory to PATH for benchmark and bombard binaries
if [ -f "$HOME/benchmark" ] || [ -f "$HOME/bombard" ]; then
    export PATH="$HOME:$PATH"
fi

# Create persistent env file
BOMBARD_LINE=""
if [ -f "$HOME/bombard" ]; then
    BOMBARD_LINE="export EVMBOMBARD_PATH=\"\$HOME/bombard\""
fi

cat > "$HOME/.benchmark-env" <<EOF
# Avalanche Benchmark Environment
export AVALANCHEGO_PATH="\$HOME/.avalanchego/avalanchego"
export AVALANCHEGO_PLUGIN_DIR="\$HOME/.avalanchego/plugins"
${BOMBARD_LINE}
export PATH="\$HOME:\$PATH"
EOF

echo "✓ Environment variables set:"
echo "  AVALANCHEGO_PATH=$AVALANCHEGO_PATH"
echo "  AVALANCHEGO_PLUGIN_DIR=$AVALANCHEGO_PLUGIN_DIR"
if [ -n "$EVMBOMBARD_PATH" ]; then
    echo "  EVMBOMBARD_PATH=$EVMBOMBARD_PATH"
fi
echo ""
echo "To persist across sessions, add to ~/.bashrc:"
echo "  echo 'source ~/.benchmark-env' >> ~/.bashrc"
