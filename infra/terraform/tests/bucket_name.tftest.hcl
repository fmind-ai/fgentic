mock_provider "google" {
  override_during = plan

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

run "accepts_shortest_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "a-1"
  }
}

run "accepts_longest_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "a12345678901234567890123456789012345678901234567890123456789012"
  }
}

run "rejects_empty_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = ""
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_short_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "ab"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_long_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "a123456789012345678901234567890123456789012345678901234567890123"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_uppercase_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "Fgentic-backups"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_underscore_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "fgentic_backups"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_dotted_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "backups.example.com"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_hyphen_prefixed_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "-fgentic-backups"
  }

  expect_failures = [var.pg_backups_bucket_name]
}

run "rejects_hyphen_suffixed_backup_bucket_name" {
  command = plan

  variables {
    pg_backups_bucket_name = "fgentic-backups-"
  }

  expect_failures = [var.pg_backups_bucket_name]
}
