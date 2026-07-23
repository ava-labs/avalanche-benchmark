terraform {
  required_providers {
    aws  = { source = "hashicorp/aws", version = "~> 5.0" }
    http = { source = "hashicorp/http" }
  }
}

# Two-region failover topology:
#   site A (primary) + control  -> us-west-1
#   site B (backup)             -> us-west-2
# Every resource is tagged Project=avalanche-benchmark (teardown/safety filters key on it).

provider "aws" {
  region = "us-west-1" # site A + control
  default_tags { tags = local.common_tags }
}

provider "aws" {
  alias  = "usw2"
  region = "us-west-2" # site B
  default_tags { tags = local.common_tags }
}

# Public IP of the machine running Terraform (the operator), for SSH ingress.
data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  config   = yamldecode(file("${path.module}/config.yaml"))
  prefix   = local.config.prefix
  owner    = local.config.owner
  app_name = "benchmark"
  common_tags = {
    Owner   = local.owner
    Project = "avalanche-benchmark"
  }
  # Per-DC machine counts (first-class config). Terraform only provisions N boxes
  # per region; roles (validator/spare/RPC) are assigned later in the reconcile/.env
  # layer. site_b_count = 0 => no EC2 spend in us-west-2 (the us-west-2 SG + key pair
  # are free and stay; the backup_site_node_ips output collapses to ""). Defaults keep
  # the validated 6-box site (3 validators + 1 spare + 2 archive RPCs).
  site_a_count = try(local.config.site_a_count, 6)
  site_b_count = try(local.config.site_b_count, 0)
  public_key   = file(pathexpand(local.config.public_key_path))
  operator_ip  = "${chomp(data.http.my_ip.response_body)}/32"
}

# The org SCP evaluates every resource created by RunInstances. volume_tags must
# carry common_tags explicitly or the root EBS volume causes the whole launch to
# be denied even though the instance itself has the provider default tags.

# No IAM instance profile: the benchmark nodes need no AWS API access, and the org
# SCP denies iam:CreateRole for this role anyway. (The previous role had no policies
# attached, so it granted nothing.)

# ---- Key pairs (one per region; same public key) ----------------------------
resource "aws_key_pair" "a" {
  key_name   = "${local.prefix}-key"
  public_key = local.public_key
}

resource "aws_key_pair" "b" {
  provider   = aws.usw2
  key_name   = "${local.prefix}-key"
  public_key = local.public_key
}

# ---- AMIs (per region; IDs differ) ------------------------------------------
data "aws_ami" "ubuntu_a" {
  most_recent = true
  owners      = ["099720109477"] # Canonical
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
}

data "aws_ami" "ubuntu_b" {
  provider    = aws.usw2
  most_recent = true
  owners      = ["099720109477"]
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
}

# ---- Control box (us-west-1): runs the P-chain primaries + orchestration ----
# Its own SG (no dependency on the node SGs) so the node SGs can reference its IP
# for SSH without a cycle. Avalanchego ports are open broadly so cross-region
# nodes can bootstrap the primary network off it.
resource "aws_security_group" "control" {
  name        = "${local.prefix}-${local.app_name}-control"
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

resource "aws_security_group_rule" "control_avax" {
  type              = "ingress"
  from_port         = 9650
  to_port           = 9700
  protocol          = "tcp"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Avalanchego API/staking/primary-bootstrap (cross-region)"
}

resource "aws_security_group_rule" "control_egress" {
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.control.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "All outbound"
}

resource "aws_instance" "control" {
  ami                    = data.aws_ami.ubuntu_a.id
  instance_type          = "m6a.4xlarge"
  key_name               = aws_key_pair.a.key_name
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
  tags        = { Name = "${local.prefix}-control" }
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-control-root" })
}

# ---- Per-site security groups -----------------------------------------------
# SSH from the operator AND the control box; avalanchego ports open broadly so
# node<->node P2P works cross-region and nodes can reach the control primaries.
resource "aws_security_group" "site_a" {
  name        = "${local.prefix}-${local.app_name}-a"
  description = "Benchmark site A nodes"
}

resource "aws_security_group_rule" "a_ssh" {
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = aws_security_group.site_a.id
  cidr_blocks       = [local.operator_ip, "${aws_instance.control.public_ip}/32"]
  description       = "SSH from operator + control"
}

resource "aws_security_group_rule" "a_avax" {
  type              = "ingress"
  from_port         = 9650
  to_port           = 9700
  protocol          = "tcp"
  security_group_id = aws_security_group.site_a.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Avalanchego API/staking/P2P (cross-region)"
}

resource "aws_security_group_rule" "a_egress" {
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.site_a.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "All outbound"
}

resource "aws_security_group" "site_b" {
  provider    = aws.usw2
  name        = "${local.prefix}-${local.app_name}-b"
  description = "Benchmark site B nodes"
}

resource "aws_security_group_rule" "b_ssh" {
  provider          = aws.usw2
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  security_group_id = aws_security_group.site_b.id
  cidr_blocks       = [local.operator_ip, "${aws_instance.control.public_ip}/32"]
  description       = "SSH from operator + control"
}

resource "aws_security_group_rule" "b_avax" {
  provider          = aws.usw2
  type              = "ingress"
  from_port         = 9650
  to_port           = 9700
  protocol          = "tcp"
  security_group_id = aws_security_group.site_b.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "Avalanchego API/staking/P2P (cross-region)"
}

resource "aws_security_group_rule" "b_egress" {
  provider          = aws.usw2
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.site_b.id
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "All outbound"
}

# ---- Nodes ------------------------------------------------------------------
resource "aws_instance" "site_a" {
  count                  = local.site_a_count
  ami                    = data.aws_ami.ubuntu_a.id
  instance_type          = "m6a.4xlarge" # 16 vCPU, 64GB RAM, AMD EPYC
  key_name               = aws_key_pair.a.key_name
  vpc_security_group_ids = [aws_security_group.site_a.id]

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
  tags        = { Name = "${local.prefix}-a${count.index + 1}" }
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-a${count.index + 1}-root" })
}

resource "aws_instance" "site_b" {
  provider               = aws.usw2
  count                  = local.site_b_count # 0 => single-DC (no us-west-2 EC2 spend); set site_b_count in config.yaml for two-site
  ami                    = data.aws_ami.ubuntu_b.id
  instance_type          = "m6a.4xlarge"
  key_name               = aws_key_pair.b.key_name
  vpc_security_group_ids = [aws_security_group.site_b.id]

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
  tags        = { Name = "${local.prefix}-b${count.index + 1}" }
  volume_tags = merge(local.common_tags, { Name = "${local.prefix}-b${count.index + 1}-root" })
}

# ---- Outputs (formatted for _common.sh / .env) ------------------------------
output "control_ip" {
  value = aws_instance.control.public_ip
}

output "node_ips" {
  description = "NODE_IPS — site A, comma-separated"
  value       = join(",", aws_instance.site_a[*].public_ip)
}

output "backup_site_node_ips" {
  description = "BACKUP_SITE_NODE_IPS — site B, comma-separated"
  value       = join(",", aws_instance.site_b[*].public_ip)
}
