# Playbook 07: run with a connected P-chain

This playbook runs the fleet with a P-chain node that follows the public
network. This is the `follow` mode. The default mode is `frozen`
([01-provision.md](01-provision.md)); use `follow` when the machines have
internet egress and you want live P-chain state.

## When to use which mode

| Question | frozen (default) | follow |
|---|---|---|
| Internet egress necessary after deploy? | no | yes, to `PCHAIN_API` |
| `l1 set-weight` works live? | no, unfreeze first | yes |
| P-chain archive necessary? | yes (`pchain archive`) | no |
| New `l1 create` visible to the fleet? | after an unfreeze cycle | immediately after replay |
| Failover drills, key swaps, load tests | identical | identical |

The L1 itself is the same chain in both modes. Only the P-chain node's
upstream connection differs.

## Procedure

```bash
./bin/l1 keygen                    # step 1: make the node identities
./bin/l1 create                    # step 2: register the L1 on the public network
./bin/fleet deploy follow          # step 3: deploy the full fleet, P-chain follows
./bin/fleet status                 # step 4: confirm the result
```

There is no archive step and no freeze step. In step 4, wait until the
P-chain MODE is `synced` and every node is `up`.

## Operation notes

- The P-chain node replays the public network's blocks continuously. Its
  status columns show UPSTREAM HEIGHT and LAG; a small lag is normal.
- Weight changes apply directly: `./bin/l1 set-weight <letter> <weight>`,
  then confirm with `./bin/l1 weights`.
- All drills work the same as in frozen mode: stop, destroy, start,
  place. See [05-failover-drill.md](05-failover-drill.md).
- `fleet status` reads the validator sets from the public API in this
  mode, so it needs the same egress as the P-chain node.

## Move to frozen later

A following fleet becomes an isolated fleet with three commands:

```bash
./bin/fleet status                 # wait for READY TO FREEZE = yes
./bin/fleet pchain archive         # capture ./pchain.tar.gz
./bin/fleet pchain freeze          # cut the upstream connection
```

The L1 nodes do not restart, and the chain does not stop.
