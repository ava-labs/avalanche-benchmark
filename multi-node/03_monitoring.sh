#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
REMOTE_DIR="~/avalanche-benchmark"

# Port layout per machine:
#   Primary:   HTTP 9650
#   Validator: HTTP 9652
#   RPC:       HTTP 9654

# ------------------------------------------------------------------------------
# Load configuration
# ------------------------------------------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
    echo "ERROR: .env file not found"
    exit 1
fi

source "$ENV_FILE"

if [ -z "$NODE1_IP" ] || [ -z "$NODE2_IP" ] || [ -z "$NODE3_IP" ]; then
    echo "ERROR: Missing node IPs in .env"
    exit 1
fi

echo "=== Deploying Monitoring to Node 1 ==="
echo "Prometheus + Grafana will run on: $NODE1_IP"
echo "Scraping metrics from all 9 nodes (3 machines x 3 node types)"
echo ""

# ------------------------------------------------------------------------------
# Step 1: Check binaries exist
# ------------------------------------------------------------------------------
echo "[1/4] Checking binaries..."

if [ ! -f "$SCRIPT_DIR/bin/prometheus" ]; then
    echo "ERROR: bin/prometheus not found. Run 'make deps' first."
    exit 1
fi

if [ ! -f "$SCRIPT_DIR/bin/grafana.tar.gz" ]; then
    echo "ERROR: bin/grafana.tar.gz not found. Run 'make deps' first."
    exit 1
fi

echo "  Binaries found."

# Helper: upload file only if remote differs (size check)
upload_if_changed() {
    local src="$1"
    local dest_host="$2"
    local dest_path="$3"
    local local_size=$(stat -c%s "$src" 2>/dev/null || stat -f%z "$src" 2>/dev/null)
    local remote_size=$(ssh "$dest_host" "stat -c%s '$dest_path' 2>/dev/null || echo 0")
    if [ "$local_size" != "$remote_size" ]; then
        scp -q "$src" "$dest_host:$dest_path"
        return 0  # uploaded
    fi
    return 1  # skipped
}

# ------------------------------------------------------------------------------
# Step 2: Generate prometheus.yml with all 9 nodes
# ------------------------------------------------------------------------------
echo "[2/4] Generating prometheus.yml..."

cat > "$SCRIPT_DIR/prometheus.yml" << EOF
global:
  scrape_interval: 5s
  evaluation_interval: 5s

scrape_configs:
  # Primary network nodes (port 9650)
  - job_name: 'avalanchego-primary'
    metrics_path: /ext/metrics
    static_configs:
      - targets:
          - '${NODE1_IP}:9650'
          - '${NODE2_IP}:9650'
          - '${NODE3_IP}:9650'
        labels:
          role: 'primary'

  # Validator nodes (port 9652)
  - job_name: 'avalanchego-validator'
    metrics_path: /ext/metrics
    static_configs:
      - targets:
          - '${NODE1_IP}:9652'
          - '${NODE2_IP}:9652'
          - '${NODE3_IP}:9652'
        labels:
          role: 'validator'

  # RPC nodes (port 9654)
  - job_name: 'avalanchego-rpc'
    metrics_path: /ext/metrics
    static_configs:
      - targets:
          - '${NODE1_IP}:9654'
          - '${NODE2_IP}:9654'
          - '${NODE3_IP}:9654'
        labels:
          role: 'rpc'
EOF

echo "  prometheus.yml generated (9 targets)."

# ------------------------------------------------------------------------------
# Step 3: Upload to node1
# ------------------------------------------------------------------------------
echo "[3/4] Uploading monitoring to $NODE1_IP..."

ssh "$NODE1_IP" "mkdir -p $REMOTE_DIR/grafana/provisioning/datasources $REMOTE_DIR/grafana/provisioning/dashboards $REMOTE_DIR/grafana/dashboards"

# Upload prometheus binary (skip if same size)
if upload_if_changed "$SCRIPT_DIR/bin/prometheus" "$NODE1_IP" "$REMOTE_DIR/prometheus"; then
    echo "  Uploaded prometheus"
else
    echo "  Skipped prometheus (unchanged)"
fi

scp -q "$SCRIPT_DIR/prometheus.yml" "$NODE1_IP:$REMOTE_DIR/"

# Upload grafana tarball and extract on remote (only if needed)
if upload_if_changed "$SCRIPT_DIR/bin/grafana.tar.gz" "$NODE1_IP" "$REMOTE_DIR/grafana.tar.gz"; then
    echo "  Uploaded grafana.tar.gz, extracting..."
    ssh "$NODE1_IP" "cd $REMOTE_DIR && rm -rf grafana-dist && tar -xzf grafana.tar.gz && mv grafana-v* grafana-dist && rm grafana.tar.gz"
else
    echo "  Skipped grafana (unchanged)"
fi

scp -q "$SCRIPT_DIR/grafana-datasources.yml" "$NODE1_IP:$REMOTE_DIR/grafana/provisioning/datasources/datasources.yml"
scp -q "$SCRIPT_DIR/grafana-dashboards.yml" "$NODE1_IP:$REMOTE_DIR/grafana/provisioning/dashboards/dashboards.yml"
scp -q "$SCRIPT_DIR/avalanche-dashboard.json" "$NODE1_IP:$REMOTE_DIR/grafana/dashboards/"
scp -q "$SCRIPT_DIR/start-grafana.sh" "$NODE1_IP:$REMOTE_DIR/"

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 4: Start Prometheus and Grafana
# ------------------------------------------------------------------------------
echo "[4/4] Starting Prometheus and Grafana..."

ssh "$NODE1_IP" bash << 'START_EOF'
set -e
cd ~/avalanche-benchmark

# Kill existing
pkill -f prometheus || true
pkill -f grafana || true
sleep 1

# Start Prometheus
nohup ./prometheus \
    --config.file=prometheus.yml \
    --storage.tsdb.path=data/prometheus \
    --web.listen-address=0.0.0.0:9090 \
    >/dev/null 2>&1 &
echo $! > data/prometheus.pid

# Start Grafana via wrapper script
mkdir -p data/grafana data/grafana/logs data/grafana/plugins
chmod +x start-grafana.sh
nohup ./start-grafana.sh >data/grafana.log 2>&1 &
echo $! > data/grafana.pid

sleep 3
START_EOF

# Wait for services to be ready
echo "  Waiting for services..."
for i in {1..30}; do
    if curl -sf "http://$NODE1_IP:9090/-/ready" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

for i in {1..30}; do
    if curl -sf "http://$NODE1_IP:3000/api/health" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

echo ""
echo "=== Monitoring Ready ==="
echo ""
echo "Prometheus: http://$NODE1_IP:9090"
echo "Grafana:    http://$NODE1_IP:3000/d/avalanche-benchmark/avalanche-benchmark?orgId=1&refresh=5s&from=now-5m&to=now"
echo ""
echo "Scraping 9 nodes:"
echo "  Primary (9650):   $NODE1_IP, $NODE2_IP, $NODE3_IP"
echo "  Validator (9652): $NODE1_IP, $NODE2_IP, $NODE3_IP"
echo "  RPC (9654):       $NODE1_IP, $NODE2_IP, $NODE3_IP"
echo ""
echo "Grafana has anonymous admin access enabled (no login required)."
