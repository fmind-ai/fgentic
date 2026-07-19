mock_provider "google" {
  override_during = plan

  mock_resource "google_service_account" {
    defaults = {
      email = "fgentic-pg-backup@fgentic-test-project.iam.gserviceaccount.com"
      name  = "projects/fgentic-test-project/serviceAccounts/fgentic-pg-backup@fgentic-test-project.iam.gserviceaccount.com"
    }
  }

  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id             = "fgentic-test-project"
  manage_dns             = false
  node_count             = 1
  pg_backups_bucket_name = "fgentic-test-pg-backups"
  master_authorized_networks = [
    {
      cidr_block   = "203.0.113.10/32"
      display_name = "test"
    }
  ]
}

run "enforces_backup_hard_delete_horizon" {
  command = plan

  assert {
    condition     = length(google_storage_bucket.pg_backups.soft_delete_policy) == 1 && google_storage_bucket.pg_backups.soft_delete_policy[0].retention_duration_seconds == 0
    error_message = "The backup bucket must explicitly disable Cloud Storage soft delete."
  }

  assert {
    condition     = length(google_storage_bucket.pg_backups.retention_policy) == 1 && tonumber(google_storage_bucket.pg_backups.retention_policy[0].retention_period) == 604800 && !google_storage_bucket.pg_backups.retention_policy[0].is_locked
    error_message = "The backup bucket must retain objects for seven days with a reversible, unlocked policy."
  }

  assert {
    condition     = length(google_storage_bucket.pg_backups.lifecycle_rule) == 1
    error_message = "The backup bucket must keep exactly one retention lifecycle rule."
  }

  assert {
    condition     = length(google_storage_bucket.pg_backups.lifecycle_rule[0].action) == 1 && one(google_storage_bucket.pg_backups.lifecycle_rule[0].action).type == "Delete"
    error_message = "The sole backup lifecycle action must delete expired objects."
  }

  assert {
    condition     = length(google_storage_bucket.pg_backups.lifecycle_rule[0].condition) == 1 && one(google_storage_bucket.pg_backups.lifecycle_rule[0].condition).age == 60
    error_message = "The backup lifecycle must preserve the exact 60-day hard horizon."
  }

  assert {
    condition     = google_storage_bucket.pg_backups.uniform_bucket_level_access && google_storage_bucket.pg_backups.public_access_prevention == "enforced"
    error_message = "Retention changes must preserve the backup bucket's public-access controls."
  }
}
