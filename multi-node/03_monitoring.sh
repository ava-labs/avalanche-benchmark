#!/bin/bash
set -ex

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"
REMOTE_DIR="~/avalanche-benchmark"

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
echo "Scraping metrics from: $NODE1_IP, $NODE2_IP, $NODE3_IP"
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

# ------------------------------------------------------------------------------
# Step 2: Generate prometheus.yml with actual IPs
# ------------------------------------------------------------------------------
echo "[2/4] Generating prometheus.yml..."

cat > "$SCRIPT_DIR/prometheus.yml" << EOF
global:
  scrape_interval: 5s
  evaluation_interval: 5s

scrape_configs:
  - job_name: 'avalanchego'
    metrics_path: /ext/metrics
    static_configs:
      - targets:
          - '${NODE1_IP}:9650'
          - '${NODE2_IP}:9650'
          - '${NODE3_IP}:9650'
EOF

echo "  prometheus.yml generated."

# ------------------------------------------------------------------------------
# Step 3: Upload to node1
# ------------------------------------------------------------------------------
echo "[3/4] Uploading monitoring to $NODE1_IP..."

ssh "$NODE1_IP" "mkdir -p $REMOTE_DIR/grafana/provisioning/datasources $REMOTE_DIR/grafana/provisioning/dashboards $REMOTE_DIR/grafana/dashboards"

# Upload prometheus
scp -q "$SCRIPT_DIR/bin/prometheus" "$NODE1_IP:$REMOTE_DIR/"
scp -q "$SCRIPT_DIR/prometheus.yml" "$NODE1_IP:$REMOTE_DIR/"

# Upload grafana tarball and extract on remote (much faster than copying extracted dir)
scp -q "$SCRIPT_DIR/bin/grafana.tar.gz" "$NODE1_IP:$REMOTE_DIR/"
ssh "$NODE1_IP" "cd $REMOTE_DIR && rm -rf grafana-dist && tar -xzf grafana.tar.gz && mv grafana-v* grafana-dist && rm grafana.tar.gz"
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
echo "Grafana:    http://$NODE1_IP:3000"
echo ""
echo "Grafana has anonymous admin access enabled (no login required)."
