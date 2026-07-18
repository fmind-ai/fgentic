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

run "rejects_zone_outside_configured_region" {
  command = plan

  variables {
    region = "europe-west1"
    zone   = "us-central1-a"
  }

  expect_failures = [var.zone]
}

run "regional_cluster_ignores_zone" {
  command = plan

  variables {
    regional = true
    region   = "europe-west4"
    zone     = "us-central1-a"
  }

  assert {
    condition     = local.location == "europe-west4" && google_container_cluster.cluster.location == "europe-west4"
    error_message = "A regional cluster must use region and ignore the zonal fallback."
  }
}
