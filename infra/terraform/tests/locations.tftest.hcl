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

run "accepts_zone_in_configured_region" {
  command = plan

  variables {
    region = "europe-west4"
    zone   = "europe-west4-b"
  }

  assert {
    condition     = local.location == "europe-west4-b" && google_container_cluster.cluster.location == "europe-west4-b"
    error_message = "A zonal cluster must use the configured zone."
  }
}

run "accepts_multi_segment_two_digit_region" {
  command = plan

  variables {
    region = "europe-west12"
    zone   = "europe-west12-c"
  }
}

run "accepts_ai_zone_suffix" {
  command = plan

  variables {
    region = "us-central1"
    zone   = "us-central1-ai1a"
  }
}

run "rejects_region_without_separator" {
  command = plan

  variables {
    region = "europewest1"
    zone   = "europewest1-b"
  }

  expect_failures = [var.region]
}

run "rejects_uppercase_region" {
  command = plan

  variables {
    region = "EUROPE-WEST1"
    zone   = "EUROPE-WEST1-b"
  }

  expect_failures = [var.region]
}

run "rejects_zone_outside_configured_region" {
  command = plan

  variables {
    region = "europe-west1"
    zone   = "us-central1-a"
  }

  expect_failures = [var.zone]
}

run "rejects_empty_zone_suffix" {
  command = plan

  variables {
    region = "europe-west1"
    zone   = "europe-west1-"
  }

  expect_failures = [var.zone]
}

run "rejects_non_alphanumeric_zone_suffix" {
  command = plan

  variables {
    region = "europe-west1"
    zone   = "europe-west1-b_test"
  }

  expect_failures = [var.zone]
}

run "regional_cluster_ignores_zone" {
  command = plan

  variables {
    regional = true
    region   = "europe-west4"
    zone     = "not a zone"
  }

  assert {
    condition     = local.location == "europe-west4" && google_container_cluster.cluster.location == "europe-west4"
    error_message = "A regional cluster must use region and ignore the zonal fallback."
  }
}
