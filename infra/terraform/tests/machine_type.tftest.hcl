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

run "accepts_default_machine_type" {
  command = plan

  assert {
    condition     = google_container_node_pool.primary.node_config[0].machine_type == "e2-standard-4"
    error_message = "The node pool must preserve the reference profile's e2-standard-4 default."
  }
}

run "rejects_empty_machine_type" {
  command = plan

  variables {
    machine_type = ""
  }

  expect_failures = [var.machine_type]
}

run "rejects_whitespace_only_machine_type" {
  command = plan

  variables {
    machine_type = "   "
  }

  expect_failures = [var.machine_type]
}

run "rejects_padded_machine_type" {
  command = plan

  variables {
    machine_type = " e2-standard-4 "
  }

  expect_failures = [var.machine_type]
}
