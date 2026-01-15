#!/bin/bash
set -e

echo "Setting up AvalancheGo for benchmark..."

# Configuration
AVALANCHEGO_VERSION="v1.14.1"
SUBNET_EVM_VERSION="v0.8.0"
INSTALL_DIR="$HOME/.avalanchego"
PLUGIN_DIR="$INSTALL_DIR/plugins"

# Create directories
mkdir -p "$INSTALL_DIR"
mkdir -p "$PLUGIN_DIR"

# Download AvalancheGo
echo "Downloading AvalancheGo $AVALANCHEGO_VERSION..."
AVALANCHEGO_URL="https://github.com/ava-labs/avalanchego/releases/download/${AVALANCHEGO_VERSION}/avalanchego-linux-amd64-${AVALANCHEGO_VERSION}.tar.gz"
wget -q -O /tmp/avalanchego.tar.gz "$AVALANCHEGO_URL"

echo "Extracting AvalancheGo..."
tar -xzf /tmp/avalanchego.tar.gz -C /tmp
cp /tmp/avalanchego-${AVALANCHEGO_VERSION}/avalanchego "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/avalanchego"
rm -rf /tmp/avalanchego.tar.gz /tmp/avalanchego-${AVALANCHEGO_VERSION}

# Download subnet-evm plugin
echo "Downloading subnet-evm plugin $SUBNET_EVM_VERSION..."
# Remove 'v' prefix from version for URL
VERSION_NO_V="${SUBNET_EVM_VERSION#v}"
PLUGIN_URL="https://github.com/ava-labs/subnet-evm/releases/download/${SUBNET_EVM_VERSION}/subnet-evm_${VERSION_NO_V}_linux_amd64.tar.gz"
wget -q -O /tmp/subnet-evm.tar.gz "$PLUGIN_URL"

echo "Extracting subnet-evm plugin..."
tar -xzf /tmp/subnet-evm.tar.gz -C /tmp
PLUGIN_ID="srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"
cp /tmp/subnet-evm "$PLUGIN_DIR/$PLUGIN_ID"
chmod +x "$PLUGIN_DIR/$PLUGIN_ID"
rm -rf /tmp/subnet-evm.tar.gz /tmp/subnet-evm

echo ""
echo "✓ AvalancheGo installed to: $INSTALL_DIR/avalanchego"
echo "✓ Plugin installed to: $PLUGIN_DIR/$PLUGIN_ID"
echo ""
echo "Run 'source ~/.benchmark-env' to set environment variables"
