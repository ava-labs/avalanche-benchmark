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
- No staking keys needed - avalanchego auto-generates certs if none exist
- Use only default tools (scp, ssh, curl, nohup) - no tmux/rsync

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
- SCP avalanchego binary, plugins, config to all 3 nodes
- Start node1 first (bootstrap with empty --bootstrap-ips/ids), wait healthy
- Start node2, node3 pointing to node1
- Health check all 3 via /ext/health (HTTP 200 = healthy)
- Save network-info.env with IPs and NodeIDs

### Step 2: Create L1 (Go executable or bash)
- Create subnet, create chain
- Collect NodeIDs + BLS keys from all 3 nodes
- Issue convertSubnetToL1 TX

### Step 3: Deploy Monitoring (bash)
- SCP prometheus.yml + grafana to node1
- Single Prometheus scraping all 3 nodes
- Grafana runs local-only (no auth, anonymous access)
- No internet required - fully offline

### Step 4: Run Benchmark (Go executable)
- SSH tunnel to any node's EVM RPC
- Run bombard through tunnel

---

## Progress

### Completed

| File | Description |
|------|-------------|
| `Makefile` | Downloads avalanchego v1.14.1 and subnet-evm v0.8.0 |
| `01_bootstrap_primary_network.sh` | Uploads binaries via scp, starts 3-node network, waits for health, saves network-info.env |
| `09_cleanup.sh` | Kills processes and cleans up files on all nodes |
| `tmp/main.tf` | Terraform for 3 AWS instances in Tokyo (m6a.4xlarge) |
| `node-config.json` | Basic node config |

### Next Step

**Create `02_create_l1.sh`** - This script needs to:
1. Read network-info.env (or accept IPs as args)
2. Create subnet on P-chain
3. Create chain with subnet-evm genesis
4. Collect BLS public keys from all 3 nodes
5. Convert subnet to L1 with all 3 as validators
6. Wait for chain to be ready
7. Save chain info (blockchain ID, RPC endpoint) for benchmark step
