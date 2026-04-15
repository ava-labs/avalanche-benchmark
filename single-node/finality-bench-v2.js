import { ethers } from "ethers";
import { readFileSync } from "fs";

const EWOQ_KEY = "0x56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027";
const NUM_TXS = 100;
let GAS_PRICE;
const RPC_URL = getRpcUrl();
const WS_URL = RPC_URL.replace(/^http/, "ws").replace(/\/rpc$/, "/ws");

async function main() {
    const wallet = new ethers.Wallet(EWOQ_KEY);
    const chainIdHex = await rpc("eth_chainId", []);
    const chainId = BigInt(chainIdHex);
    const nonceHex = await rpc("eth_getTransactionCount", [wallet.address, "latest"]);
    let nonce = Number(BigInt(nonceHex));

    const latestBlock = await rpc("eth_getBlockByNumber", ["latest", false]);
    const baseFee = BigInt(latestBlock.baseFeePerGas);
    GAS_PRICE = baseFee * 2n;

    console.log(`rpc: ${RPC_URL}`);
    console.log(`chain: ${chainId}  nonce: ${nonce}  baseFee: ${baseFee}  gasPrice: ${GAS_PRICE}`);

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

    // Tight send loop
    for (let i = 0; i < NUM_TXS; i++) {
        const t0 = performance.now();
        const signed = await wallet.signTransaction({
            to: wallet.address, value: 0n, nonce: nonce++,
            gasPrice: GAS_PRICE, gasLimit: 21_000n, chainId, type: 0,
        });
        const hash = ethers.keccak256(signed);
        const t1 = performance.now();

        const confirmPromise = new Promise((resolve) => waiters.set(hash, resolve));

        await rpc("eth_sendRawTransaction", [signed]);
        const t2 = performance.now();

        const t3 = await confirmPromise;

        console.log(
            `tx #${String(i + 1).padStart(3)}  sign ${(t1 - t0).toFixed(0)}ms  send ${(t2 - t1).toFixed(0)}ms  wait ${(t3 - t2).toFixed(0)}ms  total ${(t3 - t0).toFixed(0)}ms`
        );
    }

    ws.close();
    console.log("done");
    process.exit(0);
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
