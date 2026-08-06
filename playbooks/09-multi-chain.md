# Playbook 09: run more than one chain

One deployment can run several L1s. The chain count is an inventory
variable. A `chain=` tag on a `nodes.ini` line declares which chain a node
serves. A line without the tag serves the `main` chain. One P-chain node,
one management committee, and one control machine cover every chain.

Every chain is a subnet-evm chain with the same toolset behavior: create,
deploy, freeze, status, load, key swaps, weight changes, and app installs.

## Declare

```bash
cp examples/two-chains.ini nodes.ini   # main + trading, edit the host= lines
```

Rules:

- Every named chain needs at least one `role=validator` node.
- Each chain gets its own default weight ladder: the first three validators
  of the chain by node number start at 100000, the rest at 1000. A
  `weight=<n>` tag on a validator line overrides the ladder. Set the tag on
  every validator of a chain or on none.
- The names `oracle` and `management` are reserved.
- Per-chain configuration goes in `chains/<name>/`:
  `genesis-template.json`, `subnet-config.json`, and the node config
  variants `chain-config.json`, `chain-config-rpc.json`, and
  `chain-config-archive.json`. A file that is absent falls back to the
  shared default in `chains/default/`. Deploy picks the variant by node role: `rpc` and
  `oracle-rpc` get the rpc variant, `archive` gets the archive variant,
  every other role gets `chain-config.json`. The oracle chain follows the
  same rule under `chains/oracle/`.
- Give each chain a `genesis-template.json` with its own `chainId`. The
  genesis funds the same addresses on every chain, so a shared chainId
  lets a transaction replay across chains. `l1 create` refuses duplicate
  chainIds. The `subnet-config.json` fallback is safe to share.

## Create and deploy

The normal runbooks apply without changes. `l1 create` creates the
committee chain first, then every declared chain, main first. The funding
balance must cover one registration per validator on every chain.

```bash
go run ./cmd/l1 create            # all chains, one command
./bin/fleet deploy follow
./bin/fleet status                # CHAIN column shows each node's chain
```

The freeze gate waits for every chain's validator set. One frozen P-chain
snapshot serves all chains.

## Operate per chain

```bash
./bin/bombard -chain trading -rps 1000 -duration 5m   # load one chain
./bin/l1 set-weight m 100000       # the identity names its chain, no flag
./bin/fleet place m 10             # swaps stay within one chain
./bin/l1 weights                   # one table per chain
```

`fleet start`, `stop`, `destroy`, and `place` select machines by node
number, so they are chain-agnostic. `fleet status` reports every chain in
one table.

## Install an app per chain

Each chain has its own append-only upgrade history. The main chain's file
is `deployment/upgrades.json`. Every other chain uses
`deployment/upgrades-<name>.json`.

```bash
./bin/oracle upgrade                           # render the fragment
./bin/fleet upgrade upgrade.json               # install on the main chain
./bin/fleet upgrade --chain trading upgrade.json   # install on trading
```

The rules from playbook 08 apply per chain: full history to every node of
that chain before any restart, then a rolling restart of that chain only.

## Interop

Every chain runs subnet-evm with Warp enabled, so Avalanche Interchain
Messaging (ICM) works between the chains natively. The settlement-feed
app's relay (`oracle relay`) is the reference implementation in this
repository: it collects validator signatures over ACP-118 and delivers
signed messages to a destination chain. For production interop, run
[icm-relayer](https://github.com/ava-labs/icm-services) against the
chains' RPC and staking endpoints.
