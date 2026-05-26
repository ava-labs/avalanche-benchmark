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
  region = var.dc1_region
}

provider "aws" {
  alias  = "dc2"
  region = var.dc2_region
}

data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  app_name    = "benchmark"
  operator_ip = "${chomp(data.http.my_ip.response_body)}/32"

  dc1_node_ips      = aws_eip.dc1_node[*].public_ip
  dc2_node_ips      = aws_eip.dc2_node[*].public_ip
  benchmark_host_ip = local.dc1_node_ips[0]

  common_tags = {
    App = local.app_name
  }

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

data "aws_vpc" "dc1_default" {
  default = true
}

data "aws_subnets" "dc1_default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.dc1_default.id]
  }
}

data "aws_ami" "dc1_ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

data "aws_vpc" "dc2_default" {
  provider = aws.dc2
  count    = var.dc2_node_count > 0 ? 1 : 0
  default  = true
}

data "aws_subnets" "dc2_default" {
  provider = aws.dc2
  count    = var.dc2_node_count > 0 ? 1 : 0

  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.dc2_default[0].id]
  }
}

data "aws_ami" "dc2_ubuntu" {
  provider    = aws.dc2
  count       = var.dc2_node_count > 0 ? 1 : 0
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

resource "aws_iam_role" "ec2" {
  name = "${var.prefix}-${local.app_name}-ec2"

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
  name = "${var.prefix}-${local.app_name}-ec2"
  role = aws_iam_role.ec2.name

  tags = local.common_tags
}

resource "aws_security_group" "dc1" {
  name        = "${var.prefix}-${local.app_name}-dc1"
  description = "DC1 benchmark nodes"
  vpc_id      = data.aws_vpc.dc1_default.id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "SSH within benchmark security group"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    self        = true
  }

  ingress {
    description = "Avalanche P2P/RPC"
    from_port   = 9650
    to_port     = 9669
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${var.prefix}-${local.app_name}-dc1"
    DC   = "dc1"
  })
}

resource "aws_security_group" "dc2" {
  provider    = aws.dc2
  count       = var.dc2_node_count > 0 ? 1 : 0
  name        = "${var.prefix}-${local.app_name}-dc2"
  description = "DC2 benchmark nodes"
  vpc_id      = data.aws_vpc.dc2_default[0].id

  ingress {
    description = "SSH from operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  ingress {
    description = "SSH within benchmark security group"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    self        = true
  }

  ingress {
    description = "Avalanche P2P/RPC"
    from_port   = 9650
    to_port     = 9669
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "${var.prefix}-${local.app_name}-dc2"
    DC   = "dc2"
  })
}

resource "aws_eip" "dc1_node" {
  count  = var.dc1_node_count
  domain = "vpc"

  tags = merge(local.common_tags, {
    Name = "${var.prefix}-${local.app_name}-dc1-node-${count.index + 1}-eip"
    DC   = "dc1"
    Node = tostring(count.index + 1)
  })
}

resource "aws_eip" "dc2_node" {
  provider = aws.dc2
  count    = var.dc2_node_count
  domain   = "vpc"

  tags = merge(local.common_tags, {
    Name = "${var.prefix}-${local.app_name}-dc2-node-${count.index + 1}-eip"
    DC   = "dc2"
    Node = tostring(count.index + 1)
  })
}

resource "aws_instance" "dc1_node" {
  count = var.dc1_node_count

  ami                         = data.aws_ami.dc1_ubuntu.id
  instance_type               = var.instance_type
  key_name                    = var.key_name
  iam_instance_profile        = aws_iam_instance_profile.ec2.name
  subnet_id                   = sort(data.aws_subnets.dc1_default.ids)[0]
  vpc_security_group_ids      = [aws_security_group.dc1.id]
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
    Name = "${var.prefix}-${local.app_name}-dc1-node-${count.index + 1}"
    DC   = "dc1"
    Node = tostring(count.index + 1)
  })
}

resource "aws_instance" "dc2_node" {
  provider = aws.dc2
  count    = var.dc2_node_count

  ami                         = data.aws_ami.dc2_ubuntu[0].id
  instance_type               = var.instance_type
  key_name                    = var.key_name
  iam_instance_profile        = aws_iam_instance_profile.ec2.name
  subnet_id                   = sort(data.aws_subnets.dc2_default[0].ids)[0]
  vpc_security_group_ids      = [aws_security_group.dc2[0].id]
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
    Name = "${var.prefix}-${local.app_name}-dc2-node-${count.index + 1}"
    DC   = "dc2"
    Node = tostring(count.index + 1)
  })
}

resource "aws_eip_association" "dc1_node" {
  count         = var.dc1_node_count
  allocation_id = aws_eip.dc1_node[count.index].id
  instance_id   = aws_instance.dc1_node[count.index].id
}

resource "aws_eip_association" "dc2_node" {
  provider      = aws.dc2
  count         = var.dc2_node_count
  allocation_id = aws_eip.dc2_node[count.index].id
  instance_id   = aws_instance.dc2_node[count.index].id
}

output "benchmark_host_ip" {
  description = "Benchmark host IP. In Terraform AWS mode, this is always the first DC1 node."
  value       = local.benchmark_host_ip
}

output "dc1_node_ips" {
  description = "DC1 public EIPs, in node order. First node is the benchmark host by convention."
  value       = local.dc1_node_ips
}

output "dc2_node_ips" {
  description = "DC2 public EIPs, in node order."
  value       = local.dc2_node_ips
}

output "env" {
  description = "Contents for the repo-root .env file."
  value       = <<EOT
SSH_USER=ubuntu
SSH_KEY=${var.ssh_key_path}
BENCHMARK_HOST_IP=${local.benchmark_host_ip}
DC1_NODE_IPS=${join(",", local.dc1_node_ips)}
DC2_NODE_IPS=${join(",", local.dc2_node_ips)}
EOT
}
