import { ethers } from "ethers";
import { readFileSync } from "fs";

const EWOQ_KEY = "0x56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027";
const NUM_TXS = 100;
const NUM_WORKERS = 10;
const RPC_URL = getRpcUrl();

async function main() {
    const chainIdHex = await rpc("eth_chainId", []);
    const chainId = BigInt(chainIdHex);
    const latestBlock = await rpc("eth_getBlockByNumber", ["latest", false]);
    const baseFee = BigInt(latestBlock.baseFeePerGas);
    const gasPrice = baseFee < 50n ? 100n : baseFee * 2n;

    console.log(`rpc: ${RPC_URL}`);
    console.log(`chain: ${chainId}  baseFee: ${baseFee}  gasPrice: ${gasPrice}  workers: ${NUM_WORKERS}`);

    // Block listener: poll loop
    const waiters = new Map();
    let nextBlock = await getBlockNumber() + 1;

    (async () => {
        while (true) {
            const head = await getBlockNumber();
            if (nextBlock > head) continue;
            for (let b = nextBlock; b <= head; b++) {
                const block = await getBlockByNumber(b);
                if (!block) continue;
                const now = performance.now();
                for (const hash of (block.transactions || [])) {
                    const w = waiters.get(hash);
                    if (w) { waiters.delete(hash); w({ time: now, blockNumber: b }); }
                }
            }
            nextBlock = head + 1;
        }
    })();

    // Create and fund workers
    const funder = new ethers.Wallet(EWOQ_KEY);
    const fundAmount = ethers.parseEther("1");
    const workers = [];

    console.log(`funding ${NUM_WORKERS} wallets (gasPrice=${gasPrice})...`);
    for (let w = 0; w < NUM_WORKERS; w++) {
        const wallet = ethers.Wallet.createRandom();
        const nonceHex = await rpc("eth_getTransactionCount", [funder.address, "latest"]);
        const nonce = Number(BigInt(nonceHex));
        const signed = await funder.signTransaction({
            to: wallet.address, value: fundAmount,
            nonce, gasPrice, gasLimit: 21_000n, chainId, type: 0,
        });
        const hash = ethers.keccak256(signed);
        const p = new Promise((resolve) => waiters.set(hash, resolve));
        await rpc("eth_sendRawTransaction", [signed]);
        workers.push({ id: w, wallet, nonce: 0 });
        await p;
        console.log(`  funded w${w}`);
    }
    console.log("funded");

    // Pre-sign all txs
    console.log("pre-signing...");
    for (const w of workers) {
        w.txs = [];
        for (let i = 0; i < NUM_TXS; i++) {
            const signed = await w.wallet.signTransaction({
                to: w.wallet.address, value: 0n, nonce: w.nonce++,
                gasPrice, gasLimit: 21_000n, chainId, type: 0,
            });
            const hash = ethers.keccak256(signed);
            w.txs.push({ signed, hash });
        }
    }
    console.log(`pre-signed ${NUM_WORKERS * NUM_TXS} txs`);

    // Run all workers in parallel
    const testStart = performance.now();
    const workerResults = await Promise.all(workers.map((w) => runWorker(w, waiters)));
    const testEnd = performance.now();
    const totalTestTime = testEnd - testStart;

    const allResults = workerResults.flat();

    // Per-worker summary
    console.log("");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("  PER-WORKER SUMMARY");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log("  Worker │  TXs │ Time(s) │  TX/s │ Avg(ms) │ Med(ms) │ Min(ms) │ Max(ms)");
    console.log("  ───────┼──────┼─────────┼───────┼─────────┼─────────┼─────────┼────────");

    for (let i = 0; i < NUM_WORKERS; i++) {
        const wr = workerResults[i];
        const times = wr.map(r => r.totalMs);
        const sorted = [...times].sort((a, b) => a - b);
        const workerTime = wr[wr.length - 1].endMs - wr[0].startMs;
        console.log(`     ${String(i).padStart(3)} │ ${String(wr.length).padStart(4)} │ ${(workerTime / 1000).toFixed(1).padStart(7)} │ ${(wr.length / (workerTime / 1000)).toFixed(1).padStart(5)} │ ${avg(times).toFixed(0).padStart(7)} │ ${sorted[Math.floor(sorted.length / 2)].toFixed(0).padStart(7)} │ ${sorted[0].toFixed(0).padStart(7)} │ ${sorted[sorted.length - 1].toFixed(0).padStart(7)}`);
    }
    console.log("");

    // Block distribution
    const blockMap = {};
    for (const r of allResults) {
        blockMap[r.blockNumber] = (blockMap[r.blockNumber] || 0) + 1;
    }
    const blocks = Object.keys(blockMap).map(Number).sort((a, b) => a - b);
    const txPerBlock = Object.values(blockMap);
    const maxTxInBlock = Math.max(...txPerBlock);
    const minTxInBlock = Math.min(...txPerBlock);
    const avgTxInBlock = allResults.length / blocks.length;

    // Aggregate summary
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("  AGGREGATE SUMMARY");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log(`  Total TXs:           ${allResults.length}`);
    console.log(`  Total test time:     ${(totalTestTime / 1000).toFixed(2)}s`);
    console.log(`  Aggregate throughput: ${(allResults.length / (totalTestTime / 1000)).toFixed(1)} TX/s`);
    console.log(`  Blocks used:         ${blocks.length} (${blocks[0]} → ${blocks[blocks.length - 1]})`);
    console.log(`  TXs per block:       min=${minTxInBlock}, max=${maxTxInBlock}, avg=${avgTxInBlock.toFixed(1)}`);
    console.log("");

    // Block distribution (sample)
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("  BLOCK DISTRIBUTION (sample: first 15, last 15)");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log("  Block  │ TXs │ Bar");
    console.log("  ───────┼─────┼──────────────────────────────────────────────────");

    const showBlocks = blocks.length <= 30
        ? blocks
        : [...blocks.slice(0, 15), null, ...blocks.slice(-15)];

    for (const b of showBlocks) {
        if (b === null) {
            console.log(`    ...  │ ... │ ... (${blocks.length - 30} blocks omitted)`);
            continue;
        }
        const count = blockMap[b];
        const barLen = Math.max(1, Math.round((count / maxTxInBlock) * 50));
        const bar = "█".repeat(barLen);
        console.log(`  ${String(b).padStart(5)} │ ${String(count).padStart(3)} │ ${bar}`);
    }
    console.log("");

    // Percentiles
    const totalTimes = allResults.map(r => r.totalMs);
    const sendTimes = allResults.map(r => r.sendMs);
    const confirmTimes = allResults.map(r => r.waitMs);

    const sortedTotal = [...totalTimes].sort((a, b) => a - b);
    const sortedSend = [...sendTimes].sort((a, b) => a - b);
    const sortedConfirm = [...confirmTimes].sort((a, b) => a - b);

    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log(`  PERCENTILES (all ${allResults.length} TXs combined)`);
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log("  ┌────────────────────┬───────────────┬───────────────┬───────────────┐");
    console.log("  │ Metric             │  Send         │  Confirm      │  Total        │");
    console.log("  ├────────────────────┼───────────────┼───────────────┼───────────────┤");
    console.log(`  │ Min                │ ${fmtMs(sortedSend[0])} │ ${fmtMs(sortedConfirm[0])} │ ${fmtMs(sortedTotal[0])} │`);
    console.log(`  │ Avg                │ ${fmtMs(avg(sendTimes))} │ ${fmtMs(avg(confirmTimes))} │ ${fmtMs(avg(totalTimes))} │`);
    console.log(`  │ Median (P50)       │ ${fmtMs(pct(sortedSend, 50))} │ ${fmtMs(pct(sortedConfirm, 50))} │ ${fmtMs(pct(sortedTotal, 50))} │`);
    console.log(`  │ P75                │ ${fmtMs(pct(sortedSend, 75))} │ ${fmtMs(pct(sortedConfirm, 75))} │ ${fmtMs(pct(sortedTotal, 75))} │`);
    console.log(`  │ P90                │ ${fmtMs(pct(sortedSend, 90))} │ ${fmtMs(pct(sortedConfirm, 90))} │ ${fmtMs(pct(sortedTotal, 90))} │`);
    console.log(`  │ P95                │ ${fmtMs(pct(sortedSend, 95))} │ ${fmtMs(pct(sortedConfirm, 95))} │ ${fmtMs(pct(sortedTotal, 95))} │`);
    console.log(`  │ P99                │ ${fmtMs(pct(sortedSend, 99))} │ ${fmtMs(pct(sortedConfirm, 99))} │ ${fmtMs(pct(sortedTotal, 99))} │`);
    console.log(`  │ Max                │ ${fmtMs(sortedSend[sortedSend.length-1])} │ ${fmtMs(sortedConfirm[sortedConfirm.length-1])} │ ${fmtMs(sortedTotal[sortedTotal.length-1])} │`);
    console.log(`  │ Std Dev            │ ${fmtMs(stddev(sendTimes))} │ ${fmtMs(stddev(confirmTimes))} │ ${fmtMs(stddev(totalTimes))} │`);
    console.log("  └────────────────────┴───────────────┴───────────────┴───────────────┘");
    console.log("");

    process.exit(0);
}

async function runWorker(worker, waiters) {
    const results = [];
    for (let i = 0; i < worker.txs.length; i++) {
        const { signed, hash } = worker.txs[i];
        const confirmPromise = new Promise((resolve) => waiters.set(hash, resolve));

        const t0 = performance.now();
        await rpc("eth_sendRawTransaction", [signed]);
        const t1 = performance.now();

        const confirmed = await confirmPromise;

        results.push({
            workerId: worker.id, txNum: i + 1,
            sendMs: t1 - t0, waitMs: confirmed.time - t1, totalMs: confirmed.time - t0,
            blockNumber: confirmed.blockNumber, startMs: t0, endMs: confirmed.time,
        });
    }
    return results;
}

// ─── HTTP JSON-RPC ───

async function rpc(method, params) {
    const res = await fetch(RPC_URL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    });
    const json = await res.json();
    if (json.error) throw new Error(`${method}: ${json.error.message}`);
    return json.result;
}

async function getBlockNumber() {
    const hex = await rpc("eth_blockNumber", []);
    return Number(BigInt(hex));
}

async function getBlockByNumber(n) {
    try { return await rpc("eth_getBlockByNumber", ["0x" + n.toString(16), false]); }
    catch { return null; }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

function avg(arr) { return arr.reduce((a, b) => a + b, 0) / arr.length; }
function stddev(arr) {
    const mean = avg(arr);
    return Math.sqrt(arr.map(v => (v - mean) ** 2).reduce((a, b) => a + b, 0) / arr.length);
}
function pct(sorted, p) {
    const idx = Math.ceil((p / 100) * sorted.length) - 1;
    return sorted[Math.max(0, idx)];
}
function fmtMs(ms) { return (ms.toFixed(0) + " ms").padStart(13); }

// ─── RPC URL ───

function getRpcUrl() {
    for (let i = 2; i < process.argv.length; i++) {
        if (process.argv[i] === "--rpc") return process.argv[i + 1];
        if (process.argv[i].startsWith("--rpc=")) return process.argv[i].slice(6);
    }
    try { return readFileSync("./network_data/rpcs.txt", "utf8").trim().split(",")[0]; }
    catch { throw new Error("No RPC URL. Pass --rpc <url> or create ./network_data/rpcs.txt"); }
}

main().catch((e) => { console.error(e); process.exitCode = 1; });
