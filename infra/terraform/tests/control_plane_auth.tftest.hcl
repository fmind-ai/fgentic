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

run "disables_legacy_control_plane_access" {
  command = plan

  assert {
    condition     = google_container_cluster.cluster.enable_legacy_abac == false
    error_message = "The GKE control plane must authorize through IAM/RBAC without legacy ABAC grants."
  }

  assert {
    condition     = length(google_container_cluster.cluster.master_auth) == 1 && length(one(google_container_cluster.cluster.master_auth).client_certificate_config) == 1 && one(one(google_container_cluster.cluster.master_auth).client_certificate_config).issue_client_certificate == false
    error_message = "The GKE control plane must not issue a retrievable legacy cluster client certificate."
  }

  assert {
    condition     = length(google_container_cluster.cluster.master_authorized_networks_config) == 1
    error_message = "Disabling legacy credentials must preserve the authorized-network control-plane boundary."
  }
}
