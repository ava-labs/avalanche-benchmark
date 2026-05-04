# Terraform Decisions

This stack is a one-off AWS simulation of the real failover lab shape:
two data centers, five machines per data center, one extra control
machine in DC1, and public IPs only.

The two simulated DCs are fixed to `us-west-1` and `us-west-2`. We are
not trying to model AWS best-practice production networking here; the
goal is to create ten machines with a deliberately constrained network
surface so the benchmark can prove that the shipped binaries and configs
are sufficient.

Public IPs are the canonical node addresses. AvalancheGo `--public-ip`,
bootstrap addresses, SSH targets, monitoring targets, and benchmark RPC
targets should all use the same public address family. Do not mix private
and public node addresses in this Terraform path.

Outbound isolation is the important property. Nodes should only be able
to initiate Avalanche traffic to the ten node public `/32`s on the
Avalanche port range. They should not be able to reach package mirrors,
GitHub, public Avalanche networks, update services, or arbitrary internet
hosts.

Stable public IPs are intentional. The firewall allowlist and the remote
`.env` inventory both depend on the exact ten node addresses plus the
DC1 control address. If an instance is recreated and gets a different
ephemeral public IP, the peer allowlist and script inventory become
stale. Elastic IPs avoid that class of error.

The control machine is deliberately not treated as part of the isolated
validator set. It runs load generation, Prometheus, and Grafana from DC1
so benchmark traffic does not come from the operator laptop. It may have
ordinary outbound internet access; the isolation property under test is
that the ten chain nodes cannot initiate arbitrary outbound internet
connections.

Inbound ports are less important for this experiment. Operator access is
allowed for SSH, APIs, Prometheus, and Grafana so the test can be run and
observed. This does not imply that nodes can initiate outbound internet
connections; AWS security groups are stateful, so replies to operator
initiated connections do not require broad outbound egress.

Default VPCs and default subnets are used because this is a one-off test
fixture. A dedicated VPC would be cleaner, but it adds little value for
the current question. The key pair named in `config.yaml` must exist in
both AWS regions.

Remote nodes are expected to receive prebuilt artifacts from the operator
machine. They should not install software during the run. Builds and
downloads happen before deployment; the nodes should only run the copied
binaries and configs.
