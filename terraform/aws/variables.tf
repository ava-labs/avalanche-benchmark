variable "prefix" {
  description = "Prefix for AWS resource names."
  type        = string
}

variable "key_name" {
  description = "Existing EC2 key pair name in the target AWS regions."
  type        = string
}

variable "ssh_key_path" {
  description = "Local private key path written into the generated .env."
  type        = string
}

variable "dc1_region" {
  description = "AWS region for DC1."
  type        = string
  default     = "us-west-1"
}

variable "dc2_region" {
  description = "AWS region for DC2."
  type        = string
  default     = "us-west-2"
}

variable "dc1_node_count" {
  description = "Number of DC1 L1 node machines, excluding the dedicated benchmark host."
  type        = number
  default     = 6

  validation {
    condition     = var.dc1_node_count >= 0
    error_message = "dc1_node_count must be zero or greater."
  }
}

variable "dc2_node_count" {
  description = "Number of identical DC2 machines. Use 0 for single-DC benchmarks."
  type        = number
  default     = 0

  validation {
    condition     = var.dc2_node_count >= 0
    error_message = "dc2_node_count must be zero or greater."
  }
}

variable "instance_type" {
  description = "EC2 instance type for all benchmark machines."
  type        = string
  default     = "i7i.4xlarge"
}
