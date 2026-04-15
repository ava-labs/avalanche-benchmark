import { ethers } from "ethers";
import { readFileSync } from "fs";

const EWOQ_KEY = "0x56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027";
const NUM_TXS = 100;
const NUM_WORKERS = 10;
const RPC_URL = getRpcUrl();
const WS_URL = RPC_URL.replace(/^http/, "ws").replace(/\/rpc$/, "/ws");

async function main() {
    const chainIdHex = await rpc("eth_chainId", []);
    const chainId = BigInt(chainIdHex);
    const latestBlock = await rpc("eth_getBlockByNumber", ["latest", false]);
    const baseFee = BigInt(latestBlock.baseFeePerGas);
    const gasPrice = baseFee * 2n;

    console.log(`rpc: ${RPC_URL}`);
    console.log(`chain: ${chainId}  baseFee: ${baseFee}  gasPrice: ${gasPrice}  workers: ${NUM_WORKERS}`);

    // Block listener over raw WebSocket
    const waiters = new Map();
    const ws = new WebSocket(WS_URL);
    let wsId = 1;
    const wsPending = new Map();

    await new Promise((resolve, reject) => {
        ws.onopen = resolve;
        ws.onerror = () => reject(new Error("ws connect failed"));
    });

    ws.onmessage = (event) => {
        const msg = JSON.parse(event.data);
        if (msg.id != null) {
            const p = wsPending.get(msg.id);
            if (p) { wsPending.delete(msg.id); p.resolve(msg); }
            return;
        }
        if (msg.method === "eth_subscription") {
            const head = msg.params?.result;
            if (!head?.number) return;
            const bn = Number(BigInt(head.number));
            getBlockByNumber(bn).then((block) => {
                if (!block) return;
                for (const hash of (block.transactions || [])) {
                    const w = waiters.get(hash);
                    if (w) { waiters.delete(hash); w(performance.now()); }
                }
            });
        }
    };

    const subResult = await wsSend(ws, wsPending, wsId++, "eth_subscribe", ["newHeads"]);
    console.log(`subscribed: ${subResult.result}`);

    // Create workers — each gets its own wallet and nonce
    const funder = new ethers.Wallet(EWOQ_KEY);
    const workers = [];
    for (let w = 0; w < NUM_WORKERS; w++) {
        const wallet = w === 0 ? funder : ethers.Wallet.createRandom();
        const nonceHex = await rpc("eth_getTransactionCount", [wallet.address, "latest"]);
        let nonce = Number(BigInt(nonceHex));
        workers.push({ id: w, wallet, nonce });
    }

    // Fund non-funder wallets
    if (NUM_WORKERS > 1) {
        const fundAmount = ethers.parseEther("1");
        console.log(`funding ${NUM_WORKERS - 1} wallets...`);
        for (let w = 1; w < NUM_WORKERS; w++) {
            const signed = await funder.signTransaction({
                to: workers[w].wallet.address, value: fundAmount,
                nonce: workers[0].nonce++, gasPrice, gasLimit: 21_000n, chainId, type: 0,
            });
            await rpc("eth_sendRawTransaction", [signed]);
        }
        // Wait for funding txs to be mined
        while (true) {
            const hex = await rpc("eth_getTransactionCount", [funder.address, "latest"]);
            if (Number(BigInt(hex)) >= workers[0].nonce) break;
            await sleep(50);
        }
        console.log("funded");
    }

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
    const results = await Promise.all(workers.map((w) => runWorker(w, waiters)));

    ws.close();

    // Print results
    const all = results.flat();
    for (const r of all) {
        console.log(
            `tx #${String(r.txNum).padStart(3)}  w${r.workerId}  send ${r.sendMs.toFixed(0)}ms  wait ${r.waitMs.toFixed(0)}ms  total ${r.totalMs.toFixed(0)}ms`
        );
    }
    console.log("done");
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

        const t2 = await confirmPromise;

        results.push({
            workerId: worker.id, txNum: i + 1,
            sendMs: t1 - t0, waitMs: t2 - t1, totalMs: t2 - t0,
        });
    }
    return results;
}

function wsSend(ws, pending, id, method, params) {
    return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        ws.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
    });
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

async function getBlockByNumber(n) {
    try { return await rpc("eth_getBlockByNumber", ["0x" + n.toString(16), false]); }
    catch { return null; }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

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
