terraform {
  required_providers {
    aws  = { source = "hashicorp/aws", version = "~> 5.0" }
    http = { source = "hashicorp/http" }
  }
}

# Fuji-anchored single-site topology (FUJI_PLAN.md item 5), one region:
#   control box (public: SSH from operator, Grafana/Prometheus)
#   validators + spare: NO external connectivity; p2p only to/from the fleet SGs
#   RPC tier: exactly ONE external egress, the pinned public Fuji peer
#     (fuji_upstream_cidr:9651; keep in lockstep with FUJI_UPSTREAM_IPS in
#     _common.sh/.env, and re-check genesis/bootstrappers.json on every
#     avalanchego bump: the hardcoded bootstrapper IPs rotate between releases).
# DNS (Route53 resolver) and NTP (Amazon Time Sync, 169.254.169.123) are
# link-local and exempt from SG filtering, so the locked tiers keep working.
# Nodes carry public IPs (default VPC) but their SGs drop everything external;
# ALL fleet traffic (.env IP lists, --public-ip, ssh from control) uses the
# PRIVATE addresses, so the SG-reference rules actually match.
#
# Two-site mode (site B) is not expressed here yet; replicate the node SGs +
# instances in a second region when the two-site drill moves to Fuji.
#
# Every resource is tagged with the configured project/owner (spend audit +
# teardown filters key on it).

provider "aws" {
  region = local.region
  default_tags { tags = { Project = local.project, Owner = local.owner } }
}

# Public IP of the machine running Terraform (the operator), for SSH ingress.
data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  config = yamldecode(file("${path.module}/config.yaml"))
  prefix = local.config.prefix
  owner  = local.config.owner # required by org SCP: instances must carry an Owner tag
  # Distinct project tag per deployment so parallel clusters in one account are
  # separable (the live failover cluster uses "avalanche-benchmark").
  project = try(local.config.project, "avalanche-benchmark")
  region  = try(local.config.region, "us-west-2")

  # Per-role machine counts: the SGs are role-shaped, so terraform must know
  # which box is which. Keep these in lockstep with VALIDATOR_IPS/SPARE_IPS/
  # RPC_IPS in .env (03_deploy assigns roles by list order).
  validator_count = try(local.config.validator_count, 3)
  spare_count     = try(local.config.spare_count, 1)
  rpc_count       = try(local.config.rpc_count, 2)

  # The ONE allowed external destination for the RPC tier: the pinned public
  # Fuji bootstrap peer (identity NodeID-pinned by the TLS handshake; the SG
  # only constrains the destination).
  fuji_upstream_cidr = try(local.config.fuji_upstream_cidr, "18.192.93.241/32")
  fuji_upstream_port = try(local.config.fuji_upstream_port, 9651)

  instance_type = try(local.config.instance_type, "m6a.4xlarge")
  public_key    = file(pathexpand(local.config.public_key_path))
  operator_ip   = "${chomp(data.http.my_ip.response_body)}/32"

  # L1 node ports (cmd/reconcile/instance.go: http 9652, staking 9653; no
  # co-location on this layout so no +10 strides).
  http_port    = 9652
  staking_port = 9653
}

# No IAM instance profile: the benchmark nodes need no AWS API access, and the
# org SCP denies iam:CreateRole for this role anyway.

resource "aws_key_pair" "fleet" {
  key_name   = "${local.prefix}-key"
  public_key = local.public_key
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical
  filter {
    name = "name"
    # 26.04 so the target glibc is >= the build box's (binaries are built on
    # the operator machine and shipped in the kit).
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-*-26.04-amd64-server-*"]
  }
}

# ---- Control box --------------------------------------------------------------
# Runs the orchestration scripts, bombard, and Prometheus/Grafana. Holds no L1
# node. Needs open egress (public Fuji API for create-l1/fund, node provisioning).
resource "aws_security_group" "control" {
  name        = "${local.prefix}-control"
  description = "Benchmark control box"
}

resource "aws_security_group_rule" "control_ssh" {
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = [local.operator_ip]
  description       = "SSH from operator"
}

resource "aws_security_group_rule" "control_grafana" {
  type              = "ingress"
  from_port         = 3000
  to_port           = 3000
  protocol          = "tcp"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Grafana (anonymous viewer for the team)"
}

resource "aws_security_group_rule" "control_prometheus" {
  type              = "ingress"
  from_port         = 9090
  to_port           = 9090
  protocol          = "tcp"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Prometheus"
}

resource "aws_security_group_rule" "control_egress" {
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "All outbound (public Fuji API, provisioning)"
}

# ---- Validator/spare tier: zero external connectivity ------------------------
resource "aws_security_group" "validator" {
  name        = "${local.prefix}-validator"
  description = "L1 validators + hot spare: p2p only within the fleet"
}

# ---- RPC tier: one external egress (the pinned Fuji peer) --------------------
resource "aws_security_group" "rpc" {
  name        = "${local.prefix}-rpc"
  description = "Pinned RPC trackers: fleet p2p + one Fuji upstream"
}

# SSH for provisioning: reconcile drives everything from the control box over
# the private network. Operator IP included as a break-glass path.
resource "aws_security_group_rule" "node_ssh" {
  for_each                 = { validator = aws_security_group.validator.id, rpc = aws_security_group.rpc.id }
  type                     = "ingress"
  from_port                = 22
  to_port                  = 22
  protocol                 = "tcp"
  security_group_id        = each.value
  source_security_group_id = aws_security_group.control.id
  description              = "SSH from control box"
}

resource "aws_security_group_rule" "node_ssh_operator" {
  for_each          = { validator = aws_security_group.validator.id, rpc = aws_security_group.rpc.id }
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks       = [local.operator_ip]
  description       = "SSH from operator (break-glass)"
}

# HTTP (9652) from the control box only: health checks, reconcile gates,
# Prometheus scrapes, bombard ingress.
resource "aws_security_group_rule" "node_http" {
  for_each                 = { validator = aws_security_group.validator.id, rpc = aws_security_group.rpc.id }
  type                     = "ingress"
  from_port                = local.http_port
  to_port                  = local.http_port
  protocol                 = "tcp"
  security_group_id        = each.value
  source_security_group_id = aws_security_group.control.id
  description              = "Node HTTP from control (health/scrape/bombard)"
}

# Staking p2p (9653) within the fleet, in all four SG directions.
resource "aws_security_group_rule" "p2p_ingress" {
  for_each = {
    val_from_val = { sg = aws_security_group.validator.id, src = aws_security_group.validator.id }
    val_from_rpc = { sg = aws_security_group.validator.id, src = aws_security_group.rpc.id }
    rpc_from_val = { sg = aws_security_group.rpc.id, src = aws_security_group.validator.id }
    rpc_from_rpc = { sg = aws_security_group.rpc.id, src = aws_security_group.rpc.id }
  }
  type                     = "ingress"
  from_port                = local.staking_port
  to_port                  = local.staking_port
  protocol                 = "tcp"
  security_group_id        = each.value.sg
  source_security_group_id = each.value.src
  description              = "Avalanche p2p within the fleet"
}

resource "aws_security_group_rule" "p2p_egress" {
  for_each = {
    val_to_val = { sg = aws_security_group.validator.id, dst = aws_security_group.validator.id }
    val_to_rpc = { sg = aws_security_group.validator.id, dst = aws_security_group.rpc.id }
    rpc_to_val = { sg = aws_security_group.rpc.id, dst = aws_security_group.validator.id }
    rpc_to_rpc = { sg = aws_security_group.rpc.id, dst = aws_security_group.rpc.id }
  }
  type                     = "egress"
  from_port                = local.staking_port
  to_port                  = local.staking_port
  protocol                 = "tcp"
  security_group_id        = each.value.sg
  source_security_group_id = each.value.dst
  description              = "Avalanche p2p within the fleet"
}

# THE one external hole: RPC tier -> pinned public Fuji bootstrap peer.
# Expected identity NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB (TLS-enforced).
resource "aws_security_group_rule" "rpc_fuji_upstream" {
  type              = "egress"
  from_port         = local.fuji_upstream_port
  to_port           = local.fuji_upstream_port
  protocol          = "tcp"
  security_group_id = aws_security_group.rpc.id
  cidr_blocks       = [local.fuji_upstream_cidr]
  description       = "P-chain follow-only upstream (pinned public Fuji peer)"
}

# ---- Instances ----------------------------------------------------------------
locals {
  node_defs = merge(
    { for i in range(local.validator_count) : "v${i + 1}" => aws_security_group.validator.id },
    { for i in range(local.spare_count) : "s${i + 1}" => aws_security_group.validator.id },
    { for i in range(local.rpc_count) : "r${i + 1}" => aws_security_group.rpc.id },
  )
}

resource "aws_instance" "control" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = local.instance_type
  key_name               = aws_key_pair.fleet.key_name
  vpc_security_group_ids = [aws_security_group.control.id]

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  root_block_device {
    volume_size = 200
    volume_type = "gp3"
    iops        = 6000
    throughput  = 500
  }
  tags = { Name = "${local.prefix}-control" }
}

resource "aws_instance" "node" {
  for_each               = local.node_defs
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = local.instance_type
  key_name               = aws_key_pair.fleet.key_name
  vpc_security_group_ids = [each.value]

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  root_block_device {
    volume_size = 200
    volume_type = "gp3"
    iops        = 6000
    throughput  = 500
  }
  tags = { Name = "${local.prefix}-${each.key}" }
}

# ---- Outputs (formatted for .env: PRIVATE addresses, fleet-internal) ----------
output "control_ip" {
  value = aws_instance.control.public_ip
}

output "control_private_ip" {
  value = aws_instance.control.private_ip
}

output "validator_ips" {
  description = "VALIDATOR_IPS (private)"
  value       = join(",", [for i in range(local.validator_count) : aws_instance.node["v${i + 1}"].private_ip])
}

output "spare_ips" {
  description = "SPARE_IPS (private)"
  value       = join(",", [for i in range(local.spare_count) : aws_instance.node["s${i + 1}"].private_ip])
}

output "rpc_ips" {
  description = "RPC_IPS (private)"
  value       = join(",", [for i in range(local.rpc_count) : aws_instance.node["r${i + 1}"].private_ip])
}
