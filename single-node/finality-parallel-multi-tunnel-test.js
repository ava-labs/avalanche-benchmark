//mod of the original bench sent by the client
import { ethers } from "ethers";
import { readFileSync } from "fs";

const EWOQ_KEY = "0x56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027";
const TXS_PER_WORKER = 10;
const TX_VALUE = ethers.parseEther("0.0001");
const FUND_AMOUNT = ethers.parseEther("1");

function getRpcUrls() {
    const data = readFileSync("./network_data/rpcs.txt", "utf8").trim();
    const urls = data.split(",").filter(Boolean);
    if (urls.length === 0) throw new Error("network_data/rpcs.txt is empty");
    return urls;
}

async function main() {
    const rpcUrls = getRpcUrls();
    const mainUrl = rpcUrls[0];
    const mainProvider = new ethers.JsonRpcProvider(mainUrl);
    const funder = new ethers.Wallet(EWOQ_KEY, mainProvider);
    const network = await mainProvider.getNetwork();

    const NUM_WORKERS = 10;

    console.log("╔═══════════════════════════════════════════════════════════╗");
    console.log("║  PARALLEL FINALITY TEST — SEPARATE CONNECTIONS           ║");
    console.log("║  Each worker: own RPC endpoint + own wallet + serial TXs ║");
    console.log("╚═══════════════════════════════════════════════════════════╝");
    console.log("");
    console.log("  Chain ID:", network.chainId.toString());
    console.log("  RPC endpoints:", rpcUrls.length, "(round-robin across workers)");
    console.log("  Workers:", NUM_WORKERS);
    console.log("  TXs per worker:", TXS_PER_WORKER);
    console.log("  Total TXs:", NUM_WORKERS * TXS_PER_WORKER);
    console.log("");
    console.log("  Connections:");
    for (let i = 0; i < NUM_WORKERS; i++) {
        const url = rpcUrls[i % rpcUrls.length];
        console.log(`    Worker ${i}: ${url}`);
    }
    console.log("");

    // Step 1: Create wallets, round-robin across RPC endpoints
    console.log(`  Step 1: Creating ${NUM_WORKERS} wallets (round-robin across ${rpcUrls.length} endpoints)...`);
    const workers = [];
    for (let i = 0; i < NUM_WORKERS; i++) {
        const url = rpcUrls[i % rpcUrls.length];
        const provider = new ethers.JsonRpcProvider(url);
        const wallet = ethers.Wallet.createRandom().connect(provider);
        workers.push({ id: i, wallet, provider, url });
    }

    // Verify all connections work
    console.log("  Step 2: Verifying all connections...");
    for (let i = 0; i < NUM_WORKERS; i++) {
        try {
            const block = await workers[i].provider.getBlockNumber();
            console.log(`    Worker ${i}: connected, block ${block}`);
        } catch (e) {
            console.error(`    Worker ${i}: FAILED — ${e.message}`);
            console.error(`    URL: ${workers[i].url}`);
            process.exit(1);
        }
    }
    console.log(`    All ${NUM_WORKERS} connections verified.`);
    console.log("");

    // Step 3: Fund all wallets
    console.log("  Step 3: Funding wallets...");
    const funderNonce = await funder.getNonce();
    const fundTxs = [];
    for (let i = 0; i < NUM_WORKERS; i++) {
        const tx = await funder.sendTransaction({
            to: workers[i].wallet.address,
            value: FUND_AMOUNT,
            nonce: funderNonce + i,
            gasLimit: 21000,
            gasPrice: 1,
        });
        fundTxs.push(tx);
    }
    for (const tx of fundTxs) {
        await tx.wait();
    }
    console.log("    All 10 wallets funded with 1 AVAX each.");
    console.log("");

    // Step 4: Run all workers in parallel
    console.log("  Step 4: Running 10 workers in parallel (separate TCP each)...");
    console.log("");
    const testStart = performance.now();

    const workerResults = await Promise.all(
        workers.map(w => runWorker(w, TXS_PER_WORKER))
    );

    const testEnd = performance.now();
    const totalTestTime = testEnd - testStart;

    // Flatten results
    const allResults = [];
    for (const wr of workerResults) {
        allResults.push(...wr);
    }

    // Per-worker summary
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("  PER-WORKER SUMMARY");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log("  Worker │  TXs │ Time(s) │  TX/s │ Avg(ms) │ Med(ms) │ Min(ms) │ Max(ms)");
    console.log("  ───────┼──────┼─────────┼───────┼─────────┼─────────┼─────────┼────────");

    for (let i = 0; i < NUM_WORKERS; i++) {
        const wr = workerResults[i];
        const times = wr.map(r => r.totalLatency);
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
    const totalTimes = allResults.map(r => r.totalLatency);
    const sendTimes = allResults.map(r => r.sendLatency);
    const confirmTimes = allResults.map(r => r.confirmLatency);

    const sortedTotal = [...totalTimes].sort((a, b) => a - b);
    const sortedSend = [...sendTimes].sort((a, b) => a - b);
    const sortedConfirm = [...confirmTimes].sort((a, b) => a - b);

    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("  PERCENTILES (all 1000 TXs combined)");
    console.log("═══════════════════════════════════════════════════════════════════════════════════════════════");
    console.log("");
    console.log("  ┌────────────────────┬───────────────┬───────────────┬───────────────┐");
    console.log("  │ Metric             │  Sign+Send    │  Confirm      │  Total        │");
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
    console.log("  COMPARISON: 10 separate TCP tunnels vs shared single tunnel");
    console.log("  Previous (shared):  Sign+Send median ~143ms (TCP head-of-line blocking)");
    console.log("  Current (separate): Sign+Send median should be ~64ms (no contention)");
    console.log("");
}

async function runWorker(worker, numTxs) {
    const results = [];
    const target = worker.wallet.address;
    let nonce = await worker.wallet.getNonce();

    for (let i = 0; i < numTxs; i++) {
        const startMs = performance.now();

        const tx = await worker.wallet.sendTransaction({
            to: target,
            value: TX_VALUE,
            nonce: nonce + i,
            gasLimit: 21000,
            gasPrice: 1,
        });

        const sentMs = performance.now();
        const receipt = await tx.wait();
        const endMs = performance.now();

        results.push({
            workerId: worker.id,
            txNum: i + 1,
            sendLatency: sentMs - startMs,
            confirmLatency: endMs - sentMs,
            totalLatency: endMs - startMs,
            blockNumber: receipt.blockNumber,
            startMs,
            endMs,
        });
    }

    console.log(`    Worker ${worker.id} done: ${numTxs} TXs`);
    return results;
}

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

main().catch((error) => {
    console.error("Test failed:", error);
    process.exitCode = 1;
});
