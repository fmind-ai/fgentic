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

run "accepts_single_letter_cluster_name" {
  command = plan

  variables {
    cluster_name = "a"
  }
}

run "accepts_forty_character_cluster_name" {
  command = plan

  variables {
    cluster_name = "a123456789012345678901234567890123456789"
  }
}

run "rejects_empty_cluster_name" {
  command = plan

  variables {
    cluster_name = ""
  }

  expect_failures = [var.cluster_name]
}

run "rejects_forty_one_character_cluster_name" {
  command = plan

  variables {
    cluster_name = "a1234567890123456789012345678901234567890"
  }

  expect_failures = [var.cluster_name]
}

run "rejects_uppercase_cluster_name" {
  command = plan

  variables {
    cluster_name = "Fgentic"
  }

  expect_failures = [var.cluster_name]
}

run "rejects_digit_prefixed_cluster_name" {
  command = plan

  variables {
    cluster_name = "1fgentic"
  }

  expect_failures = [var.cluster_name]
}

run "rejects_hyphen_suffixed_cluster_name" {
  command = plan

  variables {
    cluster_name = "fgentic-"
  }

  expect_failures = [var.cluster_name]
}
