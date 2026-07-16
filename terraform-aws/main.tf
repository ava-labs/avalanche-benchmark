terraform {
  required_providers {
    aws  = { source = "hashicorp/aws", version = "~> 5.0" }
    http = { source = "hashicorp/http" }
  }
}

# Mainnet two-site topology:
#   us-west-1: control, a1-a4 validators, rpc_a1-rpc_a2
#   us-west-2: b1-b4 validators, rpc_b1-rpc_b2
#
# Node security groups have no default egress. Validators can dial validators,
# the control box, and TCP/9651 on RPC nodes. The last exception is required:
# master configures every validator's P-chain beacons as all four RPC nodes.
# RPC nodes can dial TCP/9651 on the fleet, the control box, and the one pinned
# mainnet upstream. Every fleet path uses exact instance public /32s because
# nodes.ini advertises public IPs. AWS security-group references authorize the
# attached ENIs' private IPs, not traffic addressed through public IPs.

provider "aws" {
  region = local.site_a_region
  default_tags {
    tags = local.common_tags
  }
}

provider "aws" {
  alias  = "site_b"
  region = local.site_b_region
  default_tags {
    tags = local.common_tags
  }
}

data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  config = yamldecode(file("${path.module}/config.yaml"))

  prefix  = local.config.prefix
  owner   = local.config.owner
  project = try(local.config.project, "avalanche-benchmark")

  site_a_region = try(local.config.site_a_region, "us-west-1")
  site_b_region = try(local.config.site_b_region, "us-west-2")

  instance_type = try(local.config.instance_type, "m6a.4xlarge")
  public_key    = file(pathexpand(local.config.public_key_path))
  operator_cidr = try(local.config.operator_cidr, "${chomp(data.http.my_ip.response_body)}/32")

  mainnet_upstream_cidr = try(local.config.mainnet_upstream_cidr, "54.232.137.108/32")
  mainnet_upstream_port = try(local.config.mainnet_upstream_port, 9651)

  node_http_from = 9650
  node_http_to   = 9750
  staking_port   = 9651

  common_tags = {
    Owner   = local.owner
    Project = local.project
  }

  site_a_defs = {
    a1     = "validator"
    a2     = "validator"
    a3     = "validator"
    a4     = "validator"
    rpc_a1 = "rpc"
    rpc_a2 = "rpc"
  }
  site_b_defs = {
    b1     = "validator"
    b2     = "validator"
    b3     = "validator"
    b4     = "validator"
    rpc_b1 = "rpc"
    rpc_b2 = "rpc"
  }
}

# No IAM instance profile is used. The seed must be staged with temporary
# credentials or a presigned URL, as described by the rebuild runbook.

resource "aws_key_pair" "site_a" {
  key_name   = "${local.prefix}-key"
  public_key = local.public_key
}

resource "aws_key_pair" "site_b" {
  provider   = aws.site_b
  key_name   = "${local.prefix}-key"
  public_key = local.public_key
}

data "aws_ami" "site_a_ubuntu" {
  most_recent = true
  owners      = ["099720109477"]
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-*-26.04-amd64-server-*"]
  }
}

data "aws_ami" "site_b_ubuntu" {
  provider    = aws.site_b
  most_recent = true
  owners      = ["099720109477"]
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-*-26.04-amd64-server-*"]
  }
}

# Security groups are deliberately rule-free here. Standalone rules below keep
# SG creation independent of instances, and cross-region rules depend on
# instance public IPs without creating a graph cycle.
resource "aws_security_group" "control" {
  name        = "${local.prefix}-control"
  description = "Benchmark control box"
}

resource "aws_security_group" "validator_a" {
  name        = "${local.prefix}-validator-a"
  description = "Site A validators"
}

resource "aws_security_group" "rpc_a" {
  name        = "${local.prefix}-rpc-a"
  description = "Site A RPC nodes"
}

resource "aws_security_group" "validator_b" {
  provider    = aws.site_b
  name        = "${local.prefix}-validator-b"
  description = "Site B validators"
}

resource "aws_security_group" "rpc_b" {
  provider    = aws.site_b
  name        = "${local.prefix}-rpc-b"
  description = "Site B RPC nodes"
}

# Control ingress is operator-only. Control egress remains open for package
# provisioning, public APIs, and fetching staged artifacts.
resource "aws_security_group_rule" "control_ingress" {
  for_each = {
    ssh        = { from = 22, to = 22 }
    grafana    = { from = 3000, to = 3000 }
    prometheus = { from = 9090, to = 9090 }
  }
  type              = "ingress"
  from_port         = each.value.from
  to_port           = each.value.to
  protocol          = "tcp"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = [local.operator_cidr]
  description       = "${each.key} from operator"
}

resource "aws_security_group_rule" "control_egress" {
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Open egress for provisioning and public APIs"
}

# Operator break-glass SSH in each region.
resource "aws_security_group_rule" "node_operator_ssh_a" {
  for_each          = { validator = aws_security_group.validator_a.id, rpc = aws_security_group.rpc_a.id }
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks       = [local.operator_cidr]
  description       = "SSH from operator"
}

resource "aws_security_group_rule" "node_operator_ssh_b" {
  provider          = aws.site_b
  for_each          = { validator = aws_security_group.validator_b.id, rpc = aws_security_group.rpc_b.id }
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks       = [local.operator_cidr]
  description       = "SSH from operator"
}

# nodes.ini and control provisioning use public IPs, so both regions authorize
# the control instance's exact public /32.
resource "aws_security_group_rule" "control_to_node_a" {
  for_each = {
    validator_ssh  = { sg = aws_security_group.validator_a.id, from = 22, to = 22 }
    rpc_ssh        = { sg = aws_security_group.rpc_a.id, from = 22, to = 22 }
    validator_http = { sg = aws_security_group.validator_a.id, from = local.node_http_from, to = local.node_http_to }
    rpc_http       = { sg = aws_security_group.rpc_a.id, from = local.node_http_from, to = local.node_http_to }
  }
  type              = "ingress"
  from_port         = each.value.from
  to_port           = each.value.to
  protocol          = "tcp"
  security_group_id = each.value.sg
  cidr_blocks       = ["${aws_instance.control.public_ip}/32"]
  description       = "Control public IP access to site A nodes"
}

resource "aws_security_group_rule" "control_to_node_b" {
  provider = aws.site_b
  for_each = {
    validator_ssh  = { sg = aws_security_group.validator_b.id, from = 22, to = 22 }
    rpc_ssh        = { sg = aws_security_group.rpc_b.id, from = 22, to = 22 }
    validator_http = { sg = aws_security_group.validator_b.id, from = local.node_http_from, to = local.node_http_to }
    rpc_http       = { sg = aws_security_group.rpc_b.id, from = local.node_http_from, to = local.node_http_to }
  }
  type              = "ingress"
  from_port         = each.value.from
  to_port           = each.value.to
  protocol          = "tcp"
  security_group_id = each.value.sg
  cidr_blocks       = ["${aws_instance.control.public_ip}/32"]
  description       = "Control access to site B nodes"
}

# Nodes may initiate traffic to the control box. Stateful SGs automatically
# permit responses to control-initiated SSH, HTTP, and scrape connections.
resource "aws_security_group_rule" "node_to_control_a" {
  for_each          = { validator = aws_security_group.validator_a.id, rpc = aws_security_group.rpc_a.id }
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = each.value
  cidr_blocks       = ["${aws_instance.control.public_ip}/32"]
  description       = "Node egress to control public IP"
}

resource "aws_security_group_rule" "node_to_control_b" {
  provider          = aws.site_b
  for_each          = { validator = aws_security_group.validator_b.id, rpc = aws_security_group.rpc_b.id }
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = each.value
  cidr_blocks       = ["${aws_instance.control.public_ip}/32"]
  description       = "Node egress to control"
}

# RPC nodes have exactly one non-fleet P2P destination.
resource "aws_security_group_rule" "rpc_mainnet_upstream_a" {
  type              = "egress"
  from_port         = local.mainnet_upstream_port
  to_port           = local.mainnet_upstream_port
  protocol          = "tcp"
  security_group_id = aws_security_group.rpc_a.id
  cidr_blocks       = [local.mainnet_upstream_cidr]
  description       = "P-chain follow-only pinned mainnet upstream"
}

resource "aws_security_group_rule" "rpc_mainnet_upstream_b" {
  provider          = aws.site_b
  type              = "egress"
  from_port         = local.mainnet_upstream_port
  to_port           = local.mainnet_upstream_port
  protocol          = "tcp"
  security_group_id = aws_security_group.rpc_b.id
  cidr_blocks       = [local.mainnet_upstream_cidr]
  description       = "P-chain follow-only pinned mainnet upstream"
}

# Instances explicitly tag both instance and root volume in RunInstances.
# Provider default_tags cover all other taggable resources as well.
resource "aws_instance" "control" {
  ami                         = data.aws_ami.site_a_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = aws_key_pair.site_a.key_name
  vpc_security_group_ids      = [aws_security_group.control.id]
  associate_public_ip_address = true

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
  tags        = merge(local.common_tags, { Name = "${local.prefix}-control" })
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-control-root" })
}

resource "aws_instance" "site_a" {
  for_each                    = local.site_a_defs
  ami                         = data.aws_ami.site_a_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = aws_key_pair.site_a.key_name
  vpc_security_group_ids      = [each.value == "validator" ? aws_security_group.validator_a.id : aws_security_group.rpc_a.id]
  associate_public_ip_address = true

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
  tags        = merge(local.common_tags, { Name = "${local.prefix}-${each.key}", Role = each.value, Site = "A" })
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-${each.key}-root", Role = each.value, Site = "A" })
}

resource "aws_instance" "site_b" {
  provider                    = aws.site_b
  for_each                    = local.site_b_defs
  ami                         = data.aws_ami.site_b_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = aws_key_pair.site_b.key_name
  vpc_security_group_ids      = [each.value == "validator" ? aws_security_group.validator_b.id : aws_security_group.rpc_b.id]
  associate_public_ip_address = true

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
  tags        = merge(local.common_tags, { Name = "${local.prefix}-${each.key}", Role = each.value, Site = "B" })
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-${each.key}-root", Role = each.value, Site = "B" })
}

# Exact public /32 rules for all fleet P2P, including same-region traffic.
# These rules are created only after EC2 has assigned every address. Both role
# groups need the full fleet on TCP/9651: validators follow every RPC as a
# P-chain beacon, and all nodes are explicit L1 state-sync peers.
resource "aws_security_group_rule" "fleet_p2p_ingress_a" {
  for_each          = { validator = aws_security_group.validator_a.id, rpc = aws_security_group.rpc_a.id }
  type              = "ingress"
  from_port         = local.staking_port
  to_port           = local.staking_port
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks = concat(
    [for instance in aws_instance.site_a : "${instance.public_ip}/32"],
    [for instance in aws_instance.site_b : "${instance.public_ip}/32"],
  )
  description = "Avalanche P2P from exact fleet public IPs"
}

resource "aws_security_group_rule" "fleet_p2p_ingress_b" {
  provider          = aws.site_b
  for_each          = { validator = aws_security_group.validator_b.id, rpc = aws_security_group.rpc_b.id }
  type              = "ingress"
  from_port         = local.staking_port
  to_port           = local.staking_port
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks = concat(
    [for instance in aws_instance.site_a : "${instance.public_ip}/32"],
    [for instance in aws_instance.site_b : "${instance.public_ip}/32"],
  )
  description = "Avalanche P2P from exact fleet public IPs"
}

resource "aws_security_group_rule" "fleet_p2p_egress_a" {
  for_each          = { validator = aws_security_group.validator_a.id, rpc = aws_security_group.rpc_a.id }
  type              = "egress"
  from_port         = local.staking_port
  to_port           = local.staking_port
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks = concat(
    [for instance in aws_instance.site_a : "${instance.public_ip}/32"],
    [for instance in aws_instance.site_b : "${instance.public_ip}/32"],
  )
  description = "Avalanche P2P to exact fleet public IPs"
}

resource "aws_security_group_rule" "fleet_p2p_egress_b" {
  provider          = aws.site_b
  for_each          = { validator = aws_security_group.validator_b.id, rpc = aws_security_group.rpc_b.id }
  type              = "egress"
  from_port         = local.staking_port
  to_port           = local.staking_port
  protocol          = "tcp"
  security_group_id = each.value
  cidr_blocks = concat(
    [for instance in aws_instance.site_a : "${instance.public_ip}/32"],
    [for instance in aws_instance.site_b : "${instance.public_ip}/32"],
  )
  description = "Avalanche P2P to exact fleet public IPs"
}

output "control_ip" {
  value = aws_instance.control.public_ip
}

output "site_a_public_ips" {
  value = { for name, instance in aws_instance.site_a : name => instance.public_ip }
}

output "site_b_public_ips" {
  value = { for name, instance in aws_instance.site_b : name => instance.public_ip }
}

output "validator_ips" {
  description = "All validator public IPs, ordered a1-a4 then b1-b4"
  value = join(",", concat(
    [for name in ["a1", "a2", "a3", "a4"] : aws_instance.site_a[name].public_ip],
    [for name in ["b1", "b2", "b3", "b4"] : aws_instance.site_b[name].public_ip],
  ))
}

output "rpc_ips" {
  description = "All RPC public IPs, ordered rpc_a1-rpc_a2 then rpc_b1-rpc_b2"
  value = join(",", concat(
    [for name in ["rpc_a1", "rpc_a2"] : aws_instance.site_a[name].public_ip],
    [for name in ["rpc_b1", "rpc_b2"] : aws_instance.site_b[name].public_ip],
  ))
}

output "nodes_ini" {
  description = "Ready-to-paste master nodes.ini inventory using cross-region-reachable public IPs"
  value = join("\n", [
    "a1     host=${aws_instance.site_a["a1"].public_ip}  role=validator  dc=A",
    "a2     host=${aws_instance.site_a["a2"].public_ip}  role=validator  dc=A",
    "a3     host=${aws_instance.site_a["a3"].public_ip}  role=validator  dc=A",
    "a4     host=${aws_instance.site_a["a4"].public_ip}  role=validator  dc=A",
    "rpc_a1 host=${aws_instance.site_a["rpc_a1"].public_ip}  role=rpc        dc=A",
    "rpc_a2 host=${aws_instance.site_a["rpc_a2"].public_ip}  role=rpc        dc=A",
    "b1     host=${aws_instance.site_b["b1"].public_ip}  role=validator  dc=B",
    "b2     host=${aws_instance.site_b["b2"].public_ip}  role=validator  dc=B",
    "b3     host=${aws_instance.site_b["b3"].public_ip}  role=validator  dc=B",
    "b4     host=${aws_instance.site_b["b4"].public_ip}  role=validator  dc=B",
    "rpc_b1 host=${aws_instance.site_b["rpc_b1"].public_ip}  role=rpc        dc=B",
    "rpc_b2 host=${aws_instance.site_b["rpc_b2"].public_ip}  role=rpc        dc=B",
  ])
}
