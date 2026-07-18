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

run "accepts_single_letter_network_name" {
  command = plan

  variables {
    network_name = "a"
  }
}

run "accepts_longest_composable_network_name" {
  command = plan

  variables {
    network_name = "a1234567890123456789012345678901234567890123456789012345"
  }

  assert {
    condition     = google_compute_subnetwork.subnet.name == "${var.network_name}-subnet" && google_compute_router.router.name == "${var.network_name}-router" && length(google_compute_subnetwork.subnet.name) == 63 && length(google_compute_router.router.name) == 63
    error_message = "The longest valid network prefix must produce exact 63-character subnet and router names."
  }
}

run "rejects_empty_network_name" {
  command = plan

  variables {
    network_name = ""
  }

  expect_failures = [var.network_name]
}

run "rejects_uncomposable_network_name" {
  command = plan

  variables {
    network_name = "a12345678901234567890123456789012345678901234567890123456"
  }

  expect_failures = [var.network_name]
}

run "rejects_uppercase_network_name" {
  command = plan

  variables {
    network_name = "Fgentic-vpc"
  }

  expect_failures = [var.network_name]
}

run "rejects_digit_prefixed_network_name" {
  command = plan

  variables {
    network_name = "1fgentic-vpc"
  }

  expect_failures = [var.network_name]
}

run "rejects_hyphen_suffixed_network_name" {
  command = plan

  variables {
    network_name = "fgentic-vpc-"
  }

  expect_failures = [var.network_name]
}
