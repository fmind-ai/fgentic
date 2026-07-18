# One-time bootstrap: the versioned GCS bucket holding the Terraform state for the main module.
# Apply this FIRST with local state (it never moves to remote state — chicken-and-egg), then
# `terraform init` the parent module, which points its gcs backend at this bucket:
#   terraform -chdir=infra/terraform/bootstrap init && terraform -chdir=infra/terraform/bootstrap apply
#   terraform -chdir=infra/terraform init -migrate-state
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }
}

variable "project_id" {
  type        = string
  description = "The GCP Project ID"
}

variable "region" {
  type        = string
  default     = "europe-west1"
  description = "Region for the state bucket"
}

variable "state_bucket_name" {
  type        = string
  default     = "fgentic-tfstate"
  description = "Globally-unique name of the Terraform state bucket (must match the parent module's backend config)"
}

provider "google" {
  project = var.project_id
  region  = var.region
}

resource "google_storage_bucket" "tfstate" {
  name                        = var.state_bucket_name
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # Pin the recovery window instead of inheriting a mutable Cloud Storage or organization default.
  soft_delete_policy {
    retention_duration_seconds = 604800 # 7 days
  }

  versioning {
    enabled = true # roll back a corrupted/destroyed state
  }

  lifecycle {
    prevent_destroy = true # the state bucket outlives everything else
  }
}

output "state_bucket" {
  value       = google_storage_bucket.tfstate.name
  description = "Configure the parent module's `backend \"gcs\"` with this bucket"
}
