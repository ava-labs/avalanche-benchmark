terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.0"
    }
  }
}

provider "aws" {
  alias  = "dc1"
  region = "us-west-1"
}

provider "aws" {
  alias  = "dc2"
  region = "us-west-2"
}

# Get the public IP of the machine running Terraform.
data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  config      = yamldecode(file("${path.module}/config.yaml"))
  prefix      = local.config.prefix
  key_name    = local.config.key_name
  app_name    = "benchmark"
  operator_ip = "${chomp(data.http.my_ip.response_body)}/32"

  dc1_count = 5
  dc2_count = 5

  instance_type = "i7i.4xlarge" # 16 vCPU, 128GB RAM, Intel Xeon (storage-optimized), 1x 3.75TB local NVMe

  dc1_node_ips = aws_eip.dc1_node[*].public_ip
  dc2_node_ips = aws_eip.dc2_node[*].public_ip
  control_ip   = aws_eip.dc1_control.public_ip
  all_node_ips = concat(local.dc1_node_ips, local.dc2_node_ips)
  all_node_cidrs = [
    for ip in local.all_node_ips : "${ip}/32"
  ]
  control_cidr = "${local.control_ip}/32"

  common_tags = {
    App = local.app_name
  }

  # Format the instance-store NVMe and mount it at /data. The drive is
  # ephemeral (wiped on stop/start), which matches the failover test
  # model: chain state is supposed to come back via P2P bootstrap, not
  # via persistent disk. Mount options are prod-reasonable: default
  # ext4 journaling kept, only atime updates disabled.
  mount_nvme_userdata = <<-EOT
    #!/bin/bash
    set -euxo pipefail

    DEV=""
    for link in /dev/disk/by-id/nvme-Amazon_EC2_NVMe_Instance_Storage_*; do
      [ -e "$link" ] || continue
      case "$link" in *-part*) continue ;; esac
      DEV=$(readlink -f "$link")
      break
    done

    if [ -z "$DEV" ]; then
      echo "No instance-store NVMe found" >&2
      exit 0
    fi

    if ! blkid "$DEV" >/dev/null 2>&1; then
      mkfs.ext4 -F -E lazy_itable_init=0,lazy_journal_init=0 -L data "$DEV"
    fi

    mkdir -p /data
    UUID=$(blkid -s UUID -o value "$DEV")
    if ! grep -q "UUID=$UUID" /etc/fstab; then
      echo "UUID=$UUID /data ext4 defaults,noatime,nodiratime,nofail 0 0" >> /etc/fstab
    fi
    mount /data
    chown ubuntu:ubuntu /data
  EOT
}

# Use the default VPC/subnet in each region for this one-off simulation.
data "aws_vpc" "dc1_default" {
  provider = aws.dc1
  default  = true
}

data "aws_vpc" "dc2_default" {
  provider = aws.dc2
  default  = true
}

data "aws_subnets" "dc1_default" {
  provider = aws.dc1

  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.dc1_default.id]
  }
}

data "aws_subnets" "dc2_default" {
  provider = aws.dc2

  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.dc2_default.id]
  }
}

# Ubuntu 24.04 AMI in each region.
data "aws_ami" "dc1_ubuntu" {
  provider    = aws.dc1
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

data "aws_ami" "dc2_ubuntu" {
  provider    = aws.dc2
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# IAM Role for EC2. The key pair named in config.yaml must exist in both regions.
resource "aws_iam_role" "ec2" {
  provider = aws.dc1
  name     = "${local.prefix}-${local.app_name}-ec2"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_instance_profile" "ec2" {
  provider = aws.dc1
  name     = "${local.prefix}-${local.app_name}-ec2"
  role     = aws_iam_role.ec2.name

  tags = local.common_tags
}

# Allocate public IPs first so SG egress can be restricted to exactly these /32s.
resource "aws_eip" "dc1_node" {
  provider = aws.dc1
  count    = local.dc1_count
  domain   = "vpc"

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1-node-${count.index + 1}-eip"
    DC   = "dc1"
    Node = tostring(count.index + 1)
  })
}

resource "aws_eip" "dc2_node" {
  provider = aws.dc2
  count    = local.dc2_count
  domain   = "vpc"

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc2-node-${count.index + 1}-eip"
    DC   = "dc2"
    Node = tostring(count.index + 1)
  })
}

resource "aws_eip" "dc1_control" {
  provider = aws.dc1
  domain   = "vpc"

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1-control-eip"
    DC   = "dc1"
    Role = "control"
  })
}

# Security groups for chain nodes: SSH from operator/control plus Avalanche
# 9650-9659 open to/from anywhere. The egress restriction is the only real
# constraint -- it prevents the chain nodes from reaching package mirrors,
# GitHub, etc., catching any accidental "apt install" in setup scripts.
# Ingress is loose (0.0.0.0/0) since this is a benchmark, not a hardened
# deployment.
resource "aws_security_group" "dc1_app" {
  provider    = aws.dc1
  name        = "${local.prefix}-${local.app_name}-dc1"
  description = "DC1 benchmark nodes - egress restricted to Avalanche ports"
  vpc_id      = data.aws_vpc.dc1_default.id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "SSH from DC1 control host"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.control_cidr]
  }

  ingress {
    description = "Avalanche P2P/RPC (open inbound, egress is the real restriction)"
    from_port   = 9650
    to_port     = 9659
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Avalanche P2P/RPC outbound only - forces air-gapped behavior on setup scripts"
    from_port   = 9650
    to_port     = 9659
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1"
    DC   = "dc1"
  })
}

resource "aws_security_group" "dc2_app" {
  provider    = aws.dc2
  name        = "${local.prefix}-${local.app_name}-dc2"
  description = "DC2 benchmark nodes - egress restricted to Avalanche ports"
  vpc_id      = data.aws_vpc.dc2_default.id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "SSH from DC1 control host"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.control_cidr]
  }

  ingress {
    description = "Avalanche P2P/RPC (open inbound, egress is the real restriction)"
    from_port   = 9650
    to_port     = 9659
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Avalanche P2P/RPC outbound only - forces air-gapped behavior on setup scripts"
    from_port   = 9650
    to_port     = 9659
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc2"
    DC   = "dc2"
  })
}

resource "aws_security_group" "dc1_control" {
  provider    = aws.dc1
  name        = "${local.prefix}-${local.app_name}-dc1-control"
  description = "DC1 failover control host"
  vpc_id      = data.aws_vpc.dc1_default.id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "Prometheus from operator"
    from_port   = 9090
    to_port     = 9090
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "Grafana from operator"
    from_port   = 3000
    to_port     = 3000
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "Avalanche P2P/RPC inbound (control hosts the 2 primary network validators)"
    from_port   = 9650
    to_port     = 9659
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # The control host runs the load generator and monitoring. It is not part of
  # the isolated validator set, so it may fetch packages or accept operator
  # tooling while the nodes themselves remain egress-restricted.
  egress {
    description = "Control host outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1-control"
    DC   = "dc1"
    Role = "control"
  })
}

resource "aws_instance" "dc1_node" {
  provider = aws.dc1
  count    = local.dc1_count

  ami                         = data.aws_ami.dc1_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = local.key_name
  iam_instance_profile        = aws_iam_instance_profile.ec2.name
  subnet_id                   = sort(data.aws_subnets.dc1_default.ids)[0]
  vpc_security_group_ids      = [aws_security_group.dc1_app.id]
  associate_public_ip_address = false

  user_data = local.mount_nvme_userdata

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

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1-node-${count.index + 1}"
    DC   = "dc1"
    Node = tostring(count.index + 1)
  })
}

resource "aws_instance" "dc2_node" {
  provider = aws.dc2
  count    = local.dc2_count

  ami                         = data.aws_ami.dc2_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = local.key_name
  iam_instance_profile        = aws_iam_instance_profile.ec2.name
  subnet_id                   = sort(data.aws_subnets.dc2_default.ids)[0]
  vpc_security_group_ids      = [aws_security_group.dc2_app.id]
  associate_public_ip_address = false

  user_data = local.mount_nvme_userdata

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

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc2-node-${count.index + 1}"
    DC   = "dc2"
    Node = tostring(count.index + 1)
  })
}

resource "aws_instance" "dc1_control" {
  provider = aws.dc1

  ami                         = data.aws_ami.dc1_ubuntu.id
  instance_type               = local.instance_type
  key_name                    = local.key_name
  iam_instance_profile        = aws_iam_instance_profile.ec2.name
  subnet_id                   = sort(data.aws_subnets.dc1_default.ids)[0]
  vpc_security_group_ids      = [aws_security_group.dc1_control.id]
  associate_public_ip_address = false

  user_data = local.mount_nvme_userdata

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

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-${local.app_name}-dc1-control"
    DC   = "dc1"
    Role = "control"
  })
}

resource "aws_eip_association" "dc1_node" {
  provider      = aws.dc1
  count         = local.dc1_count
  allocation_id = aws_eip.dc1_node[count.index].id
  instance_id   = aws_instance.dc1_node[count.index].id
}

resource "aws_eip_association" "dc2_node" {
  provider      = aws.dc2
  count         = local.dc2_count
  allocation_id = aws_eip.dc2_node[count.index].id
  instance_id   = aws_instance.dc2_node[count.index].id
}

resource "aws_eip_association" "dc1_control" {
  provider      = aws.dc1
  allocation_id = aws_eip.dc1_control.id
  instance_id   = aws_instance.dc1_control.id
}

output "control_ip" {
  description = "DC1 control host public EIP"
  value       = local.control_ip
}

output "dc1_node_ips" {
  description = "DC1 public EIPs, in node order"
  value       = local.dc1_node_ips
}

output "dc2_node_ips" {
  description = "DC2 public EIPs, in node order"
  value       = local.dc2_node_ips
}

output "all_node_ips" {
  description = "All public EIPs, DC1 first then DC2"
  value       = local.all_node_ips
}

output "remote_env" {
  description = "Paste into ../.env for the remote scripts"
  value       = <<EOT
SSH_USER=ubuntu
CONTROL_IP=${local.control_ip}
DC1_NODE_IPS=${join(",", local.dc1_node_ips)}
DC2_NODE_IPS=${join(",", local.dc2_node_ips)}
NODE_IPS=${join(",", local.all_node_ips)}
EOT
}

# Backward-compatible single-node outputs.
output "node1_ip" {
  value = local.all_node_ips[0]
}

output "node2_ip" {
  value = local.all_node_ips[1]
}

output "node3_ip" {
  value = local.all_node_ips[2]
}

output "node4_ip" {
  value = local.all_node_ips[3]
}

output "node5_ip" {
  value = local.all_node_ips[4]
}

output "node6_ip" {
  value = local.all_node_ips[5]
}

output "node7_ip" {
  value = local.all_node_ips[6]
}

output "node8_ip" {
  value = local.all_node_ips[7]
}

output "node9_ip" {
  value = local.all_node_ips[8]
}

output "node10_ip" {
  value = local.all_node_ips[9]
}
