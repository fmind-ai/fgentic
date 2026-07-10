# OPTIONAL GKE reference cluster. The Fgentic workloads are plain Kubernetes and run on ANY
# conformant cluster (k3d, kind, EKS, AKS, on-prem); only this directory is GCP-specific. Unlike
# the sibling dev.fmind repo, this reference is NOT cost-optimised — it is sized to run the full
# Matrix + agent stack comfortably (billing is not a concern for the showcase).
variable "project_id" {
  type        = string
  description = "The GCP Project ID"
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
}

variable "network_name" {
  type        = string
  default     = "fgentic-vpc"
  description = "Name of the VPC network"
}

variable "cluster_name" {
  type        = string
  default     = "fgentic"
  description = "Name of the GKE Standard cluster"
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
  default     = "fgentic-ai-pg-backups"
  description = "Globally-unique GCS bucket for CloudNativePG WAL archiving + base backups (must match infra/postgres/cluster.yaml)"
}

variable "manage_dns" {
  type        = bool
  default     = true
  description = "Create the Cloud DNS zone + platform A records (apex, chat., matrix., auth., grafana.) for var.domain"
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
}

variable "master_authorized_networks" {
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  description = "CIDRs allowed to reach the public control-plane endpoint. REQUIRED — set your /32 in a gitignored terraform.tfvars. Never 0.0.0.0/0."

  validation {
    condition     = alltrue([for n in var.master_authorized_networks : n.cidr_block != "0.0.0.0/0"])
    error_message = "master_authorized_networks must not contain 0.0.0.0/0; use a narrow CIDR (e.g. <your-ip>/32)."
  }
  validation {
    condition     = length(var.master_authorized_networks) > 0
    error_message = "Provide at least one authorized CIDR for the control-plane endpoint."
  }
}
