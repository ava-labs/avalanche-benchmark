# Multi-Node Benchmark - Conversation Log

## User Requirements

### Initial Request
All right let's do a multi-node benchmark. So how it's gonna work? We're gonna have somebody's laptop connected to the Internet and connected to nodes via SSH in the data center and since this is a pretty private project they have the nodes, three servers isolated completely. What we're building? We're building tool for exactly one purpose: for running this exact benchmark we don't want any customization, we don't want anything, we just want perfect tool that does only one thing. So the hard part is that it's all air-gapped. Laptop is not air-gapped from which this command is gonna be sent but everything else is air-gapped. This is why we want to use primitive tools that are very easy and not gonna trigger any internet connection requirements or something like that.

Overall what are we doing here? We just putting everything so first we need to create the chain through bootstrap and single node should reveal how right so we have to bootstrap the primary network there's three nodes then we're gonna issue this create subnet now then we probably as a step two will probably gonna collect everything and like node ids we need right node ips and ids I guess only ids because ip is probably gonna be in some kind of inventory file those three ips or maybe as flags I want steps can be separate right one can be bash another can be golangs executable stuff like that you propose your architecture.

**Step 1:** Bootstrap the primary network (3 nodes), run health checks until ready to accept transactions.

**Step 2:** Create L1 - create subnet, create chain, convert to L1. Need node IDs and BLS info from nodes. Three validators, no separate RPC node for now (just query validators directly).

**Step 3:** Provision Grafana - external team provides YAML. One Prometheus scraping all 3 machines. Prometheus + Grafana on node1. User connects to Grafana from laptop.

### Clarifications
- SSH key auth: handled by default terminal (bash), no special handling needed
- Three IPs: passed as CLI flags
- Avalanchego binary: needs to be copied, everything included in archive

---

## Architecture Plan

```
[Laptop] --SSH--> [Node1: Bootstrap + Prometheus + Grafana]
                  [Node2: Validator]  
                  [Node3: Validator]
```

### Step 0: Inventory
IPs passed as flags to each script/tool.

### Step 1: Bootstrap Primary Network (bash)
- SCP avalanchego binary, staking keys, configs to all 3 nodes
- Start node1 first (bootstrap), wait healthy
- Start node2, node3 pointing to node1
- Health check all 3

### Step 2: Create L1 (Go executable, runs from laptop)
- SSH tunnel to node1's P-chain RPC (9650)
- Create subnet, create chain
- Collect NodeIDs + BLS keys from all 3 via SSH tunnels
- Issue convertSubnetToL1 TX

### Step 3: Deploy Monitoring (bash)
- SCP prometheus.yml + grafana to node1
- Single Prometheus scraping all 3 nodes
- Grafana runs local-only (no auth, anonymous access)
- No internet required - fully offline

### Step 4: Run Benchmark (Go executable)
- SSH tunnel to any node's EVM RPC
- Run bombard through tunnel

### Grafana Notes
- Works completely offline/air-gapped
- Anonymous access mode = no login required
- Pre-provision dashboard JSON = shows only that dashboard
- Access via `http://node1-ip:3000` (SSH tunnel or direct)
