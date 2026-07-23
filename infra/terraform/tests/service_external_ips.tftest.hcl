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

run "blocks_service_external_ips" {
  command = plan

  assert {
    condition     = length(google_container_cluster.cluster.service_external_ips_config) == 1 && one(google_container_cluster.cluster.service_external_ips_config).enabled == false
    error_message = "The GKE control plane must reject Services that set spec.externalIPs."
  }

  assert {
    condition     = google_container_cluster.cluster.datapath_provider == "ADVANCED_DATAPATH"
    error_message = "The Service external IP restriction must preserve Dataplane V2 enforcement."
  }
}
