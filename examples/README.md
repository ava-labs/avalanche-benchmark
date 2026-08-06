# Example inventories

Copy one file to the repository root. Name the copy `nodes.ini`. Replace
the hosts with your machines. Do not change the node numbers or the roles.

The annotated reference for every field is `nodes.ini.example`. The
oracle-L1 roles in that file belong to the settlement-feed app's
multi-chain shape.

- `dev-single-host.ini`: the recommended development fleet, on one machine.
- `uat-two-dc.ini`: the two-site shape. The failover drills in
  `playbooks/` use this shape.
- `two-chains.ini`: two L1s (main and trading) on three worker machines.
  See `playbooks/09-multi-chain.md`.
