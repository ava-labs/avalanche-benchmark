import { ethers } from "ethers";
import { readFileSync } from "fs";

const EWOQ_KEY = "0x56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027";
const RPC_URL = getRpcUrl();
const WS_URL = RPC_URL.replace(/^http/, "ws").replace(/\/rpc$/, "/ws");

async function main() {
    const wallet = new ethers.Wallet(EWOQ_KEY);
    const provider = new ethers.WebSocketProvider(WS_URL);

    const network = await provider.getNetwork();
    const chainId = network.chainId;
    const nonce = await provider.getTransactionCount(wallet.address, "latest");
    let nextNonce = nonce;

    console.log(`rpc: ${RPC_URL}`);
    console.log(`ws:  ${WS_URL}`);
    console.log(`chain: ${chainId}  nonce: ${nextNonce}`);

    // Loop 1: block listener via ethers ws subscription
    provider.on("block", async (blockNumber) => {
        const block = await provider.getBlock(blockNumber);
        if (block) {
            console.log(`block ${blockNumber} txs=${block.transactions.length}`);
        }
    });

    // Loop 2: send a tx once per second
    while (true) {
        const signed = await wallet.signTransaction({
            to: wallet.address,
            value: 0n,
            nonce: nextNonce++,
            gasPrice: 25n,
            gasLimit: 21_000n,
            chainId,
            type: 0,
        });
        await provider.send("eth_sendRawTransaction", [signed]);
        console.log(`sent nonce=${nextNonce - 1}`);
        await sleep(1000);
    }
}

function sleep(ms) {
    return new Promise((r) => setTimeout(r, ms));
}

// ─── RPC URL ───

function getRpcUrl() {
    for (let i = 2; i < process.argv.length; i++) {
        if (process.argv[i] === "--rpc") return process.argv[i + 1];
        if (process.argv[i].startsWith("--rpc=")) return process.argv[i].slice(6);
    }
    try {
        return readFileSync("./network_data/rpcs.txt", "utf8").trim().split(",")[0];
    } catch {
        throw new Error("No RPC URL. Pass --rpc <url> or create ./network_data/rpcs.txt");
    }
}

main().catch((e) => {
    console.error(e);
    process.exitCode = 1;
});
