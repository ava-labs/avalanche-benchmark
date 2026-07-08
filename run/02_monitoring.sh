#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$SCRIPT_DIR/_common.sh"

# Prometheus + Grafana, run LOCALLY on this control host - NOT on a benchmark
# node. Failover monitoring has to outlive any single site: node 1 (a1) is a
# validator that goes DOWN during a site-A failover, so hosting the monitor
# there would blind you exactly when the failover happens. The control host is
# the one machine that stays up across both site outages and already reaches
# every node's :9652 (bombard does). It scrapes all 12 nodes' /ext/metrics and
# keeps recording the survivors as a site drops out.
#
#   bin/prometheus            fetched by `make monitoring-deps` (linux-amd64)
#   bin/grafana-dist/         extracted here from bin/grafana.tar.gz
#   monitoring/prometheus.yml generated here (scrape targets; gitignored)
#   monitoring/provisioning/  generated here (gitignored)
#   monitoring/dashboards/    committed dashboards (avalanche + failover)
#   data/prometheus,          runtime TSDB
#   data/grafana              runtime grafana state

PROM_BIN="$SCRIPT_DIR/bin/prometheus"
GRAFANA_TGZ="$SCRIPT_DIR/bin/grafana.tar.gz"
GRAFANA_HOME="$SCRIPT_DIR/bin/grafana-dist"
MON_DIR="$SCRIPT_DIR/monitoring"
PROM_YML="$MON_DIR/prometheus.yml"
PROV_DIR="$MON_DIR/provisioning"
DASH_DIR="$MON_DIR/dashboards"
DATA_DIR="$SCRIPT_DIR/data"
PROM_PORT=9090
GRAFANA_PORT=3000

echo "=== Monitoring (Prometheus + Grafana on this control host) ==="

# ------------------------------------------------------------------------------
# [1/5] Binaries
# ------------------------------------------------------------------------------
echo "[1/5] Checking monitoring binaries..."
if [ ! -x "$PROM_BIN" ] || [ ! -f "$GRAFANA_TGZ" ]; then
    echo "ERROR: missing bin/prometheus and/or bin/grafana.tar.gz."
    echo "       Fetch them first:  make monitoring-deps"
    exit 1
fi
if [ ! -x "$GRAFANA_HOME/bin/grafana" ]; then
    echo "  Extracting grafana..."
    rm -rf "$SCRIPT_DIR"/bin/grafana-v* "$GRAFANA_HOME"
    tar -xzf "$GRAFANA_TGZ" -C "$SCRIPT_DIR/bin"
    mv "$SCRIPT_DIR"/bin/grafana-v* "$GRAFANA_HOME"
fi
echo "  OK."

# ------------------------------------------------------------------------------
# [2/5] Generate prometheus.yml - scrape every node in both sites. Each target
# gets a friendly `instance` (so dashboards read "validator-1", "rpc-1" instead
# of an IP:port), plus site / machine / role labels for grouping. Names are the
# HOME role: after a failover a "backup-N" node is the one now validating - which
# is exactly the story the dashboards show.
# ------------------------------------------------------------------------------
echo "[2/5] Generating prometheus.yml..."
mkdir -p "$MON_DIR"

emit_target() {  # host port site machine instance role
    printf "      - targets: ['%s:%s']\n" "$1" "$2"
    printf "        labels:\n"
    printf "          instance: %s\n" "$5"
    printf "          site: %s\n" "$3"
    printf "          machine: %s\n" "$4"
    printf "          role: %s\n" "$6"
}

# Targets AND labels come ENTIRELY from `reconcile endpoints` (the single source
# of truth), so monitoring tracks whatever topology is configured - any
# validator/spare/RPC counts, any co-location - with correct per-node labels.
# No hardcoded slot count or role names. Each endpoint line is:
#   name <TAB> site <TAB> role <TAB> host <TAB> port
# where name is the machine id (a1../b1.. for stake slots, rpc_a*/rpc_b* for RPCs) and role is v1..vN / spare / rpc.
export NODE_IPS BACKUP_SITE_NODE_IPS
{
    echo "global:"
    echo "  scrape_interval: 5s"
    echo "  evaluation_interval: 5s"
    echo ""
    echo "scrape_configs:"
    echo "  - job_name: 'avalanche-l1'"
    echo "    metrics_path: /ext/metrics"
    echo "    static_configs:"
    while IFS=$'\t' read -r name site role host port; do
        [ -n "$host" ] || continue
        # Map the slot role to the dashboard's role vocabulary. Site-A validator
        # slots are live validators; the SAME slots on the backup site are
        # zero-weight syncing trackers in steady state.
        drole="$role"
        case "$role" in
        v*) [ "$site" = a ] && drole=validator || drole=tracker ;;
        spare) [ "$site" = a ] && drole=spare || drole=tracker ;;
        rpc) drole=rpc ;;
        esac
        emit_target "$host" "$port" "$site" "$name" "$name" "$drole"
    done < <("$SCRIPT_DIR/bin/benchmark-fleet" endpoints)
    # Control-host exporters: the fleet weight exporter (started below) and
    # bombard's /metrics (only up while a load run is active; a down target is
    # expected and the Load generator row on Failover Overview just stays blank).
    echo "  - job_name: 'fleet'"
    echo "    scrape_timeout: 4s"
    # The exporter emits its own instance labels (a1..b4, matching the node
    # scrape vocabulary); without honor_labels Prometheus would rename them to
    # exported_instance and stamp localhost:9091 as instance.
    echo "    honor_labels: true"
    echo "    static_configs:"
    echo "      - targets: ['localhost:9091']"
    echo "  - job_name: 'bombard'"
    echo "    scrape_timeout: 4s"
    echo "    static_configs:"
    echo "      - targets: ['localhost:9092']"
} > "$PROM_YML"

TARGET_COUNT=$(grep -c "      - targets:" "$PROM_YML")
echo "  $PROM_YML ($TARGET_COUNT targets)."

# ------------------------------------------------------------------------------
# [3/5] Grafana provisioning (datasource -> local prometheus; auto-load dashboards)
# ------------------------------------------------------------------------------
echo "[3/5] Provisioning grafana..."
mkdir -p "$PROV_DIR/datasources" "$PROV_DIR/dashboards" "$DASH_DIR"
cp "$MON_DIR/grafana-datasources.yml" "$PROV_DIR/datasources/datasources.yml"
cat > "$PROV_DIR/dashboards/dashboards.yml" <<EOF
apiVersion: 1
providers:
    - name: 'default'
      orgId: 1
      folder: ''
      type: file
      disableDeletion: false
      editable: true
      options:
          path: $DASH_DIR
EOF
echo "  OK ($(ls "$DASH_DIR"/*.json 2>/dev/null | wc -l | tr -d ' ') dashboards)."

# ------------------------------------------------------------------------------
# [4/5] (Re)start prometheus + grafana
# ------------------------------------------------------------------------------
echo "[4/5] Starting prometheus + grafana + fleet exporter..."
pkill -f "$SCRIPT_DIR/bin/prometheus" 2>/dev/null || true
pkill -f "$GRAFANA_HOME/bin/grafana" 2>/dev/null || true
pkill -f "benchmark-fleet exporter" 2>/dev/null || true
sleep 1

# Fresh Prometheus TSDB each run. When a target's labels change (e.g. switching
# from IP to friendly instance names), Prometheus keeps the OLD series under the
# old labels, so a dashboard shows both the IP-named and friendly-named lines at
# once. Wiping guarantees the board reflects only the current label set.
rm -rf "$DATA_DIR/prometheus"
mkdir -p "$DATA_DIR/prometheus" "$DATA_DIR/grafana/logs" "$DATA_DIR/grafana/plugins"

nohup "$PROM_BIN" \
    --config.file="$PROM_YML" \
    --storage.tsdb.path="$DATA_DIR/prometheus" \
    --web.listen-address="0.0.0.0:$PROM_PORT" \
    >"$DATA_DIR/prometheus.log" 2>&1 &
echo $! > "$DATA_DIR/prometheus.pid"

GF_PATHS_DATA="$DATA_DIR/grafana" \
GF_PATHS_LOGS="$DATA_DIR/grafana/logs" \
GF_PATHS_PLUGINS="$DATA_DIR/grafana/plugins" \
GF_PATHS_PROVISIONING="$PROV_DIR" \
GF_SERVER_HTTP_ADDR="0.0.0.0" \
GF_SERVER_HTTP_PORT="$GRAFANA_PORT" \
GF_AUTH_ANONYMOUS_ENABLED="true" \
GF_AUTH_ANONYMOUS_ORG_ROLE="Admin" \
GF_AUTH_DISABLE_LOGIN_FORM="true" \
    nohup "$GRAFANA_HOME/bin/grafana" server --homepath="$GRAFANA_HOME" \
    >"$DATA_DIR/grafana.log" 2>&1 &
echo $! > "$DATA_DIR/grafana.pid"

# Fleet weight exporter: fleet_desired_weight / fleet_actual_weight on :9091,
# scraped by the 'fleet' job for the stake panels.
nohup "$SCRIPT_DIR/bin/benchmark-fleet" exporter \
    >"$DATA_DIR/fleet-exporter.log" 2>&1 &
echo $! > "$DATA_DIR/fleet-exporter.pid"

# ------------------------------------------------------------------------------
# [5/5] Wait for readiness
# ------------------------------------------------------------------------------
echo "[5/5] Waiting for services..."
for _ in $(seq 1 30); do
    if curl -sf "http://localhost:$PROM_PORT/-/ready" >/dev/null 2>&1; then break; fi
    sleep 1
done
for _ in $(seq 1 30); do
    if curl -sf "http://localhost:$GRAFANA_PORT/api/health" >/dev/null 2>&1; then break; fi
    sleep 1
done

PUBLIC_IP="$(curl -fsS https://checkip.amazonaws.com 2>/dev/null | tr -d '[:space:]' || true)"
PUBLIC_IP="${PUBLIC_IP:-localhost}"
echo ""
echo "=== Monitoring ready ==="
echo "Prometheus: http://$PUBLIC_IP:$PROM_PORT"
echo "Grafana:    http://$PUBLIC_IP:$GRAFANA_PORT   (anonymous admin, no login)"
echo "  Failover Overview: http://$PUBLIC_IP:$GRAFANA_PORT/d/failover-overview?refresh=5s&from=now-15m&to=now"
echo "  Failover Details:  http://$PUBLIC_IP:$GRAFANA_PORT/d/failover?refresh=5s&from=now-15m&to=now"
echo "  Machine board:     http://$PUBLIC_IP:$GRAFANA_PORT/d/machine?refresh=5s&from=now-15m&to=now"
echo "  Benchmark board:   http://$PUBLIC_IP:$GRAFANA_PORT/d/benchmark?refresh=5s"
echo ""
echo "Scraping $TARGET_COUNT targets: every node's /ext/metrics on its per-slot port, plus the fleet exporter (:9091) and bombard (:9092) on this host."
echo "Stop with:  kill \$(cat $DATA_DIR/prometheus.pid $DATA_DIR/grafana.pid)"
