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

run "accepts_shortest_project_id" {
  command = plan

  variables {
    project_id = "a12345"
  }

  assert {
    condition     = one(google_container_cluster.cluster.workload_identity_config).workload_pool == "a12345.svc.id.goog" && google_service_account_iam_member.pg_backup_workload_identity.member == "serviceAccount:a12345.svc.id.goog[postgres/platform-pg]"
    error_message = "The shortest valid project ID must compose the exact Workload Identity pool and backup principal."
  }
}

run "accepts_longest_project_id" {
  command = plan

  variables {
    project_id = "a12345678901234567890123456789"
  }

  assert {
    condition     = one(google_container_cluster.cluster.workload_identity_config).workload_pool == "${var.project_id}.svc.id.goog" && google_service_account_iam_member.pg_backup_workload_identity.member == "serviceAccount:${var.project_id}.svc.id.goog[postgres/platform-pg]"
    error_message = "The longest valid project ID must compose the exact Workload Identity pool and backup principal."
  }
}

run "rejects_five_character_project_id" {
  command = plan

  variables {
    project_id = "a1234"
  }

  expect_failures = [var.project_id]
}

run "rejects_thirty_one_character_project_id" {
  command = plan

  variables {
    project_id = "a123456789012345678901234567890"
  }

  expect_failures = [var.project_id]
}

run "rejects_uppercase_project_id" {
  command = plan

  variables {
    project_id = "Fgentic-project"
  }

  expect_failures = [var.project_id]
}

run "rejects_digit_prefixed_project_id" {
  command = plan

  variables {
    project_id = "1fgentic-project"
  }

  expect_failures = [var.project_id]
}

run "rejects_hyphen_suffixed_project_id" {
  command = plan

  variables {
    project_id = "fgentic-project-"
  }

  expect_failures = [var.project_id]
}

run "rejects_underscore_project_id" {
  command = plan

  variables {
    project_id = "fgentic_project"
  }

  expect_failures = [var.project_id]
}
