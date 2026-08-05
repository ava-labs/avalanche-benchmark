# Playbook 08: add an app to a running chain

This playbook installs the settlement-feed contracts on a chain that
already runs. The chain is not recreated. No genesis changes.

An app's contracts can reach the chain in three ways:

1. Genesis baking. `l1 create` bakes them into a fresh chain. This is the
   default for new deployments.
2. A state upgrade (`upgrade.json`). This playbook. Use it when the chain
   already runs and the contracts must sit at their fixed addresses.
3. A normal deployment with forge, at a new address. Use it for consumer
   contracts, for example `Settlement.sol`.

## Procedure

```bash
./bin/oracle upgrade              # step 1: write ./upgrade.json (activation in 15 minutes)
cat upgrade.json                  # step 2: review it
./bin/fleet upgrade upgrade.json  # step 3: install on every node, rolling restart
```

Step 1 renders the feed's accounts (code and storage seeds) for this
deployment's feeder key. Pass a number to change the activation delay in
minutes, for example `./bin/oracle upgrade 30`.

Step 3 first copies the file to every main-L1 node. No node restarts
before every node has the file. It then restarts the nodes one at a time
and waits for each node to serve again.

After the activation time passes, verify the contracts:

```bash
FEED=0x00000000000000000000000000000000FeedF00d
cast call $FEED "decimals()(uint8)" --rpc-url http://<rpc>:9650/ext/bc/<chain-id>/rpc
```

Then start the publisher: `./bin/oracle feed http://<rpc>:9650`.

## Rules that the toolset enforces

- The activation timestamp must be in the future. Every node must restart
  with the file before that time. The default delay is 15 minutes.
- An explicit zero value (empty code, or a storage slot set to zero) is
  refused. A zero value passes the first restart and stops the node on
  the next restart after activation, because the database reads the zero
  back as absent and the configuration check fails.
- `fleet upgrade` targets the main L1. The optional oracle L1 keeps its
  own upgrade file.

## Custom upgrades

`fleet upgrade` installs any valid subnet-evm `upgrade.json`, not only the
one `oracle upgrade` renders. Write your own for precompile changes or for
your own contracts, and the same validation and rolling restart apply.
