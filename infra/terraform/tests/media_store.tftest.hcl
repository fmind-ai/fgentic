mock_provider "google" {
  override_during = plan

  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

run "synapse_media_uses_snapshot_capable_csi" {
  command = plan

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

  assert {
    condition     = google_container_cluster.cluster.addons_config[0].gce_persistent_disk_csi_driver_config[0].enabled
    error_message = "The GKE reference must enable the PD CSI driver used by standard-rwo and media snapshots."
  }
}
