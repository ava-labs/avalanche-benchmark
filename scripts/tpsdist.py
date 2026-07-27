#!/usr/bin/env python3
"""Per-second mined-tps distribution, measured from chain timestamps only.

Independent of bombard: it reads timestampMilliseconds off the blocks
themselves, so it cannot be fooled by a lagging watcher. Use it to decide
whether a throughput complaint is the chain or the load generator.

    N=2500 python3 scripts/tpsdist.py          # last 2500 blocks
    FROM=10700 python3 scripts/tpsdist.py      # every block since 10700
    U=http://host:9650/ext/bc/<id>/rpc ...     # a specific RPC

A restart, redeploy or config change leaves a multi-minute gap in the block
stream, and a window spanning it silently averages two different configs into
one meaningless distribution. Pass FROM=<first block after the change> when
comparing configs; the script refuses windows containing a gap over GAPMS
(default 60s) so the mistake is loud rather than silent.
"""
import json, os, statistics, sys, urllib.request

N = int(os.environ.get("N", "1000"))
FROM = int(os.environ.get("FROM", "0"))
GAPMS = int(os.environ.get("GAPMS", "60000"))
env = dict(l.strip().split("=", 1) for l in open(os.path.expanduser("~/avalanche-benchmark/deployment/network.env")) if "=" in l)
url = os.environ.get("U", "http://18.145.168.232:9650/ext/bc/%s/rpc" % env["CHAIN_ID"])

def rpc(p):
    r = urllib.request.Request(url, json.dumps(p).encode(), {"content-type": "application/json"})
    return json.load(urllib.request.urlopen(r, timeout=90))

tip = int(rpc({"jsonrpc": "2.0", "id": 1, "method": "eth_blockNumber", "params": []})["result"], 16)
start = max(1, FROM if FROM else tip - N + 1)
rows = []
for base in range(start, tip + 1, 100):
    end = min(base + 99, tip)
    for item in rpc([{"jsonrpc": "2.0", "id": i, "method": "eth_getBlockByNumber", "params": [hex(i), False]} for i in range(base, end + 1)]):
        b = item.get("result")
        if b and b.get("timestampMilliseconds"):
            rows.append((int(b["number"], 16), int(b["timestampMilliseconds"], 16), len(b["transactions"])))
rows.sort()
if len(rows) < 2:
    sys.exit("no blocks in range")

d = [rows[i][1] - rows[i - 1][1] for i in range(1, len(rows))]
worst = max(range(len(d)), key=lambda i: d[i])
if d[worst] > GAPMS:
    sys.exit(
        f"block {rows[worst][0]} -> {rows[worst+1][0]} has a {d[worst]/1000:.0f}s gap "
        f"(restart or redeploy). This window spans a config change and would mix two "
        f"configs into one distribution. Re-run with FROM={rows[worst+1][0]}, or raise GAPMS to override."
    )

buckets = {}
for _, ts, tx in rows:
    buckets[ts // 1000] = buckets.get(ts // 1000, 0) + tx
keys = sorted(buckets)[1:-1]          # drop partial edge seconds
v = sorted(buckets[k] for k in keys)
pc = lambda p: v[min(len(v) - 1, int(len(v) * p))]

print(f"blocks={len(rows)} range={rows[0][0]}..{rows[-1][0]} seconds={len(v)} totalTx={sum(v)}")
print()
print("per-second mined tps")
print(f"  min    {v[0]}")
for p in (0.01, 0.05, 0.10, 0.25):
    print(f"  p{int(p*100):<5} {pc(p)}")
print(f"  p50    {statistics.median(v):.0f}")
for p in (0.75, 0.90, 0.95, 0.99):
    print(f"  p{int(p*100):<5} {pc(p)}")
print(f"  max    {v[-1]}")
m = statistics.mean(v); sd = statistics.pstdev(v)
print(f"  mean   {m:.0f}   stdev {sd:.0f}   CV {sd/m*100:.0f}%   IQR {pc(.75)-pc(.25)}")
print(f"  within +/-10% of 4000: {sum(1 for x in v if 3600 <= x <= 4400)}/{len(v)} seconds")
print(f"  below 3000:            {sum(1 for x in v if x < 3000)}/{len(v)}")
print()
print("histogram (500-tps bins)")
for lo in range(0, (v[-1] // 500 + 1) * 500, 500):
    c = sum(1 for x in v if lo <= x < lo + 500)
    if c:
        print(f"  {lo:5}-{lo+499:<5} {'#' * c} {c}")

ds = sorted(d)
q = lambda p: ds[min(len(ds) - 1, int(len(ds) * p))]
print()
print(f"block delta ms  min={ds[0]} p50={statistics.median(ds):.0f} p75={q(.75)} p90={q(.9)} p99={q(.99)} max={ds[-1]}  mean={statistics.mean(ds):.1f} stdev={statistics.pstdev(ds):.0f}")
print(f"  at 25ms floor: {sum(1 for x in d if x <= 26)}/{len(d)} blocks")
