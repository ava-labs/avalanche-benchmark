#!/bin/bash
cd ~/avalanche-benchmark
export GF_PATHS_DATA="$(pwd)/data/grafana"
export GF_PATHS_LOGS="$(pwd)/data/grafana/logs"
export GF_PATHS_PLUGINS="$(pwd)/data/grafana/plugins"
export GF_PATHS_PROVISIONING="$(pwd)/grafana/provisioning"
export GF_SERVER_HTTP_ADDR="0.0.0.0"
export GF_SERVER_HTTP_PORT="3000"
export GF_AUTH_ANONYMOUS_ENABLED="true"
export GF_AUTH_ANONYMOUS_ORG_ROLE="Admin"
export GF_AUTH_DISABLE_LOGIN_FORM="true"
exec ./grafana-dist/bin/grafana server --homepath="$(pwd)/grafana-dist"
