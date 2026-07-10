terraform {
  required_version = ">= 1.9.0" # cross-variable validation in variables.tf

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }

  # Remote state in the versioned GCS bucket created by the bootstrap/ module (apply it first,
  # then `terraform init -migrate-state` here). GCS gives native state locking.
  backend "gcs" {
    bucket = "fgentic-ai-tfstate" # = bootstrap/ state_bucket_name
    prefix = "fgentic"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
