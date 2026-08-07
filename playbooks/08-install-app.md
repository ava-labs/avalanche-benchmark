# Playbook 08: add an app to a running chain

This playbook installs the settlement-feed contracts on a chain that
already runs. The chain is not recreated. No genesis changes.

The genesis is base layer only. App contracts never bake into it: a
genesis change means a new chain, and on a frozen P-chain that means a
full unfreeze, re-create, re-freeze, and redeploy cycle. An app's
contracts reach the chain in two ways:

1. A state upgrade (`upgrade.json`). This playbook. The standard path,
   and the only one that puts contracts at fixed addresses.
2. A normal deployment with forge, at a new address. Use it for consumer
   contracts, for example `Settlement.sol`.

## Procedure

```bash
./bin/fleet app list                     # step 1: see the installable apps
./bin/fleet app install settlement-feed  # step 2: render, then install with a rolling restart
```

`fleet app install` reads the app's manifest, runs the app's renderer
from the deployment root, and installs the rendered fragment through the
same code path as `fleet upgrade`. For settlement-feed the renderer is
`./bin/oracle upgrade`, and it writes `./upgrade.json` with an activation
time 15 minutes out.

The install targets exactly one chain. The target resolves in this
order: the `--chain` flag, then the manifest's `chain` field, then
`main`. The command refuses a chain that `nodes.ini` does not declare,
before the renderer runs.

The push copies the full upgrade history to every node of the target
chain. No node restarts before every node has the file. The nodes then
restart one at a time, and each node serves again before the next stops.

### The app manifest

`apps/<name>/app.json` declares the app to the installer:

- `name`: the app name. It must equal the directory name.
- `description`: one line, shown by `fleet app list`.
- `chain`: the default target chain. Optional. The default is `main`.
- `render`: the argv that writes the upgrade fragment. It runs from the
  deployment root and inherits the environment.
- `output`: the file the renderer writes, relative to the deployment
  root. Optional. The default is `upgrade.json`.

## Verify

The upgrade executes in the first block at or after the activation time.
An idle chain therefore shows no code until a transaction produces a
block. Send one transaction after the activation time, then verify the
contracts:

```bash
FEED=0x00000000000000000000000000000000FeedF00D
cast call $FEED "decimals()(uint8)" --rpc-url http://<rpc>:9650/ext/bc/<chain-id>/rpc
```

Then start the publisher: `./bin/oracle feed http://<rpc>:9650`.

## The upgrade history

The chain's upgrade file is append-only. An entry that has activated must
stay in the file, unchanged, forever. A file that lost an activated entry
stops every node that loads it.

The toolset therefore keeps the history on the control machine, in
`deployment/upgrades.json`. `fleet upgrade` treats your file as a FRAGMENT:
it appends the fragment to the history, installs the full history on every
node, and records it before any remote work. Every later deploy also
carries the history, so a fresh machine that joins after an activation
gets the activated entries automatically.

More apps stack the same way. Each app renders its own fragment, and each
`fleet upgrade` call appends one more entry. Nothing about the first app's
entry changes when the second app arrives.

## Rules that the toolset enforces

- The activation timestamp must be in the future, and it must be later
  than every entry already in the history. Every node must restart with
  the file before the activation time. The default delay is 15 minutes.
- An explicit zero value (empty code, or a storage slot set to zero) is
  refused. A zero value passes the first restart and stops the node on
  the next restart after activation, because the database reads the zero
  back as absent and the configuration check fails.
- `fleet upgrade` targets one chain per call: `--chain <name>` selects it,
  and the default is `main`. Each chain keeps its own history file. See
  playbooks/09-multi-chain.md.

## Custom upgrades

`fleet upgrade` installs any valid subnet-evm upgrade fragment, not only
the one an app manifest names. The manual path behind `fleet app install`
stays available:

```bash
./bin/oracle upgrade              # step 1: write ./upgrade.json (activation in 15 minutes)
cat upgrade.json                  # step 2: review it
./bin/fleet upgrade upgrade.json  # step 3: install on every node, rolling restart
```

Step 1 renders the feed's accounts (code and storage seeds) for this
deployment's feeder key. Pass a number to change the activation delay in
minutes, for example `./bin/oracle upgrade 30`. Use this path to review
the fragment before it lands, or to install a hand-written fragment for
precompile changes or your own contracts. The same merge, validation,
and rolling restart apply.
