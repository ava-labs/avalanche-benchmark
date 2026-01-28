terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = "ap-northeast-1" # Tokyo
}

# Get the public IP of the machine running Terraform
data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  config      = yamldecode(file("${path.module}/config.yaml"))
  prefix      = local.config.prefix
  key_name    = local.config.key_name
  app_name    = "benchmark"
  operator_ip = "${chomp(data.http.my_ip.response_body)}/32"
}

# IAM Role for EC2
resource "aws_iam_role" "ec2" {
  name = "${local.prefix}-${local.app_name}-ec2"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

resource "aws_iam_instance_profile" "ec2" {
  name = "${local.prefix}-${local.app_name}-ec2"
  role = aws_iam_role.ec2.name
}

# Security Group - only allows traffic from operator IP
resource "aws_security_group" "app" {
  name        = "${local.prefix}-${local.app_name}"
  description = "Benchmark nodes - isolated, operator access only"

  # SSH - operator only
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  # Avalanche HTTP API - operator only
  ingress {
    from_port   = 9650
    to_port     = 9650
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  # Avalanche Staking - operator only
  ingress {
    from_port   = 9651
    to_port     = 9651
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  # Prometheus - operator only
  ingress {
    from_port   = 9090
    to_port     = 9090
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  # Grafana - operator only
  ingress {
    from_port   = 3000
    to_port     = 3000
    protocol    = "tcp"
    cidr_blocks = [local.operator_ip]
  }

  # ICMP - operator only
  ingress {
    from_port   = -1
    to_port     = -1
    protocol    = "icmp"
    cidr_blocks = [local.operator_ip]
  }

  # Internal traffic between benchmark nodes (private IPs)
  ingress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }

  # Egress - operator only (no internet access)
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [local.operator_ip]
  }

  # Allow internal egress between nodes (private IPs)
  egress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }

  tags = {
    Name = "${local.prefix}-${local.app_name}"
  }
}

# Allow inter-node traffic via public IPs (created after instances exist)
resource "aws_security_group_rule" "node_ingress" {
  count             = 3
  type              = "ingress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.app.id
  cidr_blocks       = ["${aws_instance.node[count.index].public_ip}/32"]
  description       = "Allow ingress from node ${count.index + 1} public IP"
}

resource "aws_security_group_rule" "node_egress" {
  count             = 3
  type              = "egress"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.app.id
  cidr_blocks       = ["${aws_instance.node[count.index].public_ip}/32"]
  description       = "Allow egress to node ${count.index + 1} public IP"
}

# Ubuntu 24.04 AMI
data "aws_ami" "ubuntu" {
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

# EC2 Instances - 3 nodes with 64GB RAM
resource "aws_instance" "node" {
  count = 3

  ami                  = data.aws_ami.ubuntu.id
  instance_type        = "m6a.4xlarge" # 16 vCPU, 64GB RAM, AMD EPYC
  key_name             = local.key_name
  iam_instance_profile = aws_iam_instance_profile.ec2.name
  security_groups      = [aws_security_group.app.name]

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

  tags = {
    Name = "${local.prefix}-${local.app_name}-node-${count.index + 1}"
  }
}

# Public IPs - for operator access (SSH, API)
output "node1_ip" {
  value = aws_instance.node[0].public_ip
}

output "node2_ip" {
  value = aws_instance.node[1].public_ip
}

output "node3_ip" {
  value = aws_instance.node[2].public_ip
}
