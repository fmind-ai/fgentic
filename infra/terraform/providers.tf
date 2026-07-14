terraform {
  required_version = ">= 1.9.0" # cross-variable validation in variables.tf

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }

  # Remote state in the versioned GCS bucket created by the bootstrap/ module (apply it first).
  # The bucket name is deployment-specific, so it is NOT hardcoded here — pass it at init:
  #   terraform init -backend-config="bucket=<your state_bucket_name>" -migrate-state
  # (use the bucket from bootstrap/'s `state_bucket` output). GCS gives native state locking.
  backend "gcs" {
    prefix = "fgentic"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
