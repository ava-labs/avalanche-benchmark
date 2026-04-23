#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

# Port layout per machine:
#   Primary:   HTTP 9650
#   Validator: HTTP 9652
#   RPC:       HTTP 9654

echo "=== Deploying Monitoring to Node 1 ==="
echo "Prometheus + Grafana will run on: $BOOTSTRAP_IP"
echo "Scraping metrics from $NODE_COUNT validator node(s) (port 9652)"
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
    local remote_size=$(ssh "$SSH_USER@$dest_host" "stat -c%s '$dest_path' 2>/dev/null || echo 0")
    if [ "$local_size" != "$remote_size" ]; then
        scp -q "$src" "$SSH_USER@$dest_host:$dest_path"
        return 0  # uploaded
    fi
    return 1  # skipped
}

# ------------------------------------------------------------------------------
# Step 2: Generate prometheus.yml with dynamic targets
# ------------------------------------------------------------------------------
echo "[2/4] Generating prometheus.yml..."

{
    echo "global:"
    echo "  scrape_interval: 5s"
    echo "  evaluation_interval: 5s"
    echo ""
    echo "scrape_configs:"
    echo "  - job_name: 'avalanchego-validator'"
    echo "    metrics_path: /ext/metrics"
    echo "    static_configs:"
    echo "      - targets:"
    for NODE_IP in "${NODE_IPS_ARRAY[@]}"; do
        echo "          - '${NODE_IP}:9652'"
    done
} > "$SCRIPT_DIR/prometheus.yml"

echo "  prometheus.yml generated ($NODE_COUNT validator target(s))."

# Generate grafana-dashboards.yml with correct user path
cat > "$SCRIPT_DIR/grafana-dashboards.yml" << EOF
apiVersion: 1

providers:
    - name: "default"
      orgId: 1
      folder: ""
      type: file
      disableDeletion: false
      editable: true
      options:
          path: /home/$SSH_USER/avalanche-benchmark/grafana/dashboards
EOF

echo "  grafana-dashboards.yml generated (path: /home/$SSH_USER/...)."

# ------------------------------------------------------------------------------
# Step 3: Upload to bootstrap node
# ------------------------------------------------------------------------------
echo "[3/4] Uploading monitoring to $BOOTSTRAP_IP..."

ssh "$SSH_USER@$BOOTSTRAP_IP" "mkdir -p $REMOTE_DIR/grafana/provisioning/datasources $REMOTE_DIR/grafana/provisioning/dashboards $REMOTE_DIR/grafana/dashboards"

# Upload prometheus binary (skip if same size)
if upload_if_changed "$SCRIPT_DIR/bin/prometheus" "$BOOTSTRAP_IP" "$REMOTE_DIR/prometheus"; then
    echo "  Uploaded prometheus"
else
    echo "  Skipped prometheus (unchanged)"
fi

scp -q "$SCRIPT_DIR/prometheus.yml" "$SSH_USER@$BOOTSTRAP_IP:$REMOTE_DIR/"

# Upload grafana tarball and extract on remote (only if needed)
if upload_if_changed "$SCRIPT_DIR/bin/grafana.tar.gz" "$BOOTSTRAP_IP" "$REMOTE_DIR/grafana.tar.gz"; then
    echo "  Uploaded grafana.tar.gz, extracting..."
    ssh "$SSH_USER@$BOOTSTRAP_IP" "cd $REMOTE_DIR && rm -rf grafana-dist && tar -xzf grafana.tar.gz && mv grafana-v* grafana-dist && rm grafana.tar.gz"
else
    echo "  Skipped grafana (unchanged)"
fi

scp -q "$SCRIPT_DIR/grafana-datasources.yml" "$SSH_USER@$BOOTSTRAP_IP:$REMOTE_DIR/grafana/provisioning/datasources/datasources.yml"
scp -q "$SCRIPT_DIR/grafana-dashboards.yml" "$SSH_USER@$BOOTSTRAP_IP:$REMOTE_DIR/grafana/provisioning/dashboards/dashboards.yml"
scp -q "$SCRIPT_DIR/avalanche-dashboard.json" "$SSH_USER@$BOOTSTRAP_IP:$REMOTE_DIR/grafana/dashboards/"
scp -q "$SCRIPT_DIR/start-grafana.sh" "$SSH_USER@$BOOTSTRAP_IP:$REMOTE_DIR/"

echo "  Upload complete."

# ------------------------------------------------------------------------------
# Step 4: Start Prometheus and Grafana
# ------------------------------------------------------------------------------
echo "[4/4] Starting Prometheus and Grafana..."

ssh "$SSH_USER@$BOOTSTRAP_IP" bash << 'START_EOF'
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
    if curl -sf "http://$BOOTSTRAP_IP:9090/-/ready" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

for i in {1..30}; do
    if curl -sf "http://$BOOTSTRAP_IP:3000/api/health" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

echo ""
echo "=== Monitoring Ready ==="
echo ""
echo "Prometheus: http://$BOOTSTRAP_IP:9090"
echo "Grafana:    http://$BOOTSTRAP_IP:3000/d/avalanche-benchmark/avalanche-benchmark?orgId=1&refresh=5s&from=now-5m&to=now"
echo ""
echo "Scraping $NODE_COUNT validator node(s) (port 9652):"
for NODE_IP in "${NODE_IPS_ARRAY[@]}"; do
    echo "  $NODE_IP:9652"
done
echo ""
echo "Grafana has anonymous admin access enabled (no login required)."
