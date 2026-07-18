# OPTIONAL GKE reference cluster. The Fgentic workloads are plain Kubernetes and run on ANY
# conformant cluster (k3d, kind, EKS, AKS, on-prem); only this directory is GCP-specific. Unlike
# the sibling dev.fmind repo, this reference is NOT cost-optimised — it is sized to run the full
# Matrix + agent stack comfortably (billing is not a concern for the showcase).
variable "project_id" {
  type        = string
  description = "The GCP Project ID"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be 6-30 characters, use only lowercase letters, numbers, and hyphens, start with a letter, and end with a letter or number."
  }
}

variable "region" {
  type        = string
  description = "Region for regional resources (subnet, static IP)"
  default     = "europe-west1"
}

variable "zone" {
  type        = string
  description = "Zone for the zonal GKE cluster + node pool"
  default     = "europe-west1-b"

  validation {
    condition     = var.regional || startswith(var.zone, "${var.region}-")
    error_message = "For a zonal cluster, zone must belong to region so GKE can use the regional subnet (for example, region=europe-west1 and zone=europe-west1-b)."
  }
}

variable "network_name" {
  type        = string
  default     = "fgentic-vpc"
  description = "Name of the VPC network"

  validation {
    condition     = length(var.network_name) <= 56 && can(regex("^[a-z]([a-z0-9-]*[a-z0-9])?$", var.network_name))
    error_message = "network_name must be 1-56 characters, use only lowercase letters, numbers, and hyphens, start with a letter, and end with a letter or number so derived resource names remain valid."
  }
}

variable "cluster_name" {
  type        = string
  default     = "fgentic"
  description = "Name of the GKE Standard cluster"

  validation {
    condition     = length(var.cluster_name) <= 40 && can(regex("^[a-z]([a-z0-9-]*[a-z0-9])?$", var.cluster_name))
    error_message = "cluster_name must be 1-40 characters, use only lowercase letters, numbers, and hyphens, start with a letter, and end with a letter or number."
  }
}

variable "machine_type" {
  type        = string
  default     = "e2-standard-4" # 4 vCPU / 16 GiB — headroom for Synapse + MAS + agents + Postgres
  description = "Machine type for the node pool"
}

variable "node_count" {
  type        = number
  default     = 2
  description = "Nodes in the pool (2 gives the full stack room; raise for HA/headroom)"

  validation {
    condition     = var.node_count >= 1 && floor(var.node_count) == var.node_count
    error_message = "node_count must be a positive whole number; regional clusters create this many nodes per zone."
  }
}

variable "deletion_protection" {
  type        = bool
  default     = false
  description = "Block terraform destroy of the cluster (false for a disposable reference)"
}

variable "regional" {
  type        = bool
  default     = false
  description = "Regional control plane (SLA, zone-upgrade resilience) instead of zonal; node_count becomes per-zone"
}

variable "pg_backups_bucket_name" {
  type        = string
  default     = "fgentic-pg-backups"
  description = "Globally-unique GCS bucket for CloudNativePG WAL archiving + base backups (must match infra/postgres/cluster.yaml)"
}

variable "manage_dns" {
  type        = bool
  default     = true
  description = "Create the Cloud DNS zone + platform A records (apex, chat., matrix., auth., id., grafana.) for var.domain"
}

variable "admin_console_dns_enabled" {
  type        = bool
  default     = false
  description = "Create the opt-in admin.<domain> A record; set this only when the GitOps admin_console profile is enabled"

  validation {
    condition     = !var.admin_console_dns_enabled || var.manage_dns
    error_message = "admin_console_dns_enabled requires manage_dns=true; otherwise manage the admin host in the external DNS provider."
  }
}

variable "domain" {
  type        = string
  default     = "fgentic.fmind.ai"
  description = "The platform apex domain (the Matrix server_name) — must match the cluster's platform-settings"
}

variable "dns_zone_name" {
  type        = string
  default     = "fgentic"
  description = "Name of the Cloud DNS managed zone Terraform creates for var.domain"

  validation {
    condition     = length(var.dns_zone_name) <= 63 && can(regex("^[a-z]([a-z0-9-]*[a-z0-9])?$", var.dns_zone_name))
    error_message = "dns_zone_name must be 1-63 characters, use only lowercase letters, numbers, and hyphens, start with a letter, and end with a letter or number."
  }
}

variable "master_authorized_networks" {
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  description = "IPv4 CIDRs allowed to reach the public control-plane endpoint. REQUIRED — set your /32 in a gitignored terraform.tfvars. Never an all-address /0."

  validation {
    condition = alltrue([
      for network in var.master_authorized_networks :
      try(can(cidrnetmask(network.cidr_block)) && tonumber(split("/", network.cidr_block)[1]) >= 24, false)
    ])
    error_message = "master_authorized_networks must contain valid IPv4 CIDRs no broader than /24 (for example, an office /24 or operator /32)."
  }
  validation {
    condition     = length(var.master_authorized_networks) > 0
    error_message = "Provide at least one authorized CIDR for the control-plane endpoint."
  }
  validation {
    condition     = length(var.master_authorized_networks) <= 100
    error_message = "GKE private clusters accept at most 100 master authorized networks."
  }
}
