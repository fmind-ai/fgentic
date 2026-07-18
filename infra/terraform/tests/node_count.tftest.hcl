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

run "accepts_positive_whole_node_count" {
  command = plan
}

run "rejects_zero_node_count" {
  command = plan

  variables {
    node_count = 0
  }

  expect_failures = [var.node_count]
}

run "rejects_negative_node_count" {
  command = plan

  variables {
    node_count = -1
  }

  expect_failures = [var.node_count]
}

run "rejects_fractional_node_count" {
  command = plan

  variables {
    node_count = 1.5
  }

  expect_failures = [var.node_count]
}
