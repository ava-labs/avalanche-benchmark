#!/usr/bin/env python3
"""Serve fleet_actual_weight for the failover dashboard.

Reads deployment/placement.json (machine slot -> identity, control-side truth
that `fleet place` rewrites) and deployment/public.json (identity -> registered
weight) on every scrape, so a key swap shows up on the next scrape with no
P-chain access. Run on the control host from the deployment root:

    ./monitoring/fleet-weight-exporter.py [deployment-dir] [port]
"""
import http.server
import json
import os
import sys

DEPLOYMENT = sys.argv[1] if len(sys.argv) > 1 else "deployment"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9091


def render() -> str:
    with open(os.path.join(DEPLOYMENT, "placement.json")) as f:
        placement = json.load(f)
    with open(os.path.join(DEPLOYMENT, "public.json")) as f:
        public = json.load(f)
    by_identity = {n["identity"]: n for n in public.get("nodes", [])}
    lines = [
        "# HELP fleet_actual_weight Registered validator weight per machine slot, from placement.json x public.json.",
        "# TYPE fleet_actual_weight gauge",
    ]
    for machine, identity in sorted(placement.items(), key=lambda kv: int(kv[0])):
        node = by_identity.get(identity)
        if node is None or node.get("role") != "validator":
            continue
        lines.append(
            'fleet_actual_weight{machine="%s",identity="%s"} %d'
            % (machine, identity, node["weight"])
        )
    return "\n".join(lines) + "\n"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        try:
            body = render().encode()
        except Exception as exc:  # surface config problems in the scrape
            self.send_response(500)
            self.end_headers()
            self.wfile.write(str(exc).encode())
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    http.server.HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
