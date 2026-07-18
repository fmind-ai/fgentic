mock_provider "google" {
  override_during = plan
}

run "pins_state_bucket_recovery_window" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "fgentic-test-tfstate"
  }

  assert {
    condition     = length(google_storage_bucket.tfstate.soft_delete_policy) == 1 && google_storage_bucket.tfstate.soft_delete_policy[0].retention_duration_seconds == 604800
    error_message = "The Terraform state bucket must pin a seven-day soft-delete recovery window."
  }

  assert {
    condition     = length(google_storage_bucket.tfstate.versioning) == 1 && google_storage_bucket.tfstate.versioning[0].enabled
    error_message = "The Terraform state bucket must keep object versioning enabled."
  }

  assert {
    condition     = google_storage_bucket.tfstate.uniform_bucket_level_access && google_storage_bucket.tfstate.public_access_prevention == "enforced"
    error_message = "State recovery changes must preserve the bucket's public-access controls."
  }
}
