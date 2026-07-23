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

run "pins_bounded_subnet_flow_logs" {
  command = plan

  assert {
    condition = (
      length(google_compute_subnetwork.subnet.log_config) == 1 &&
      google_compute_subnetwork.subnet.log_config[0].aggregation_interval == "INTERVAL_10_MIN" &&
      google_compute_subnetwork.subnet.log_config[0].flow_sampling == 0.5 &&
      google_compute_subnetwork.subnet.log_config[0].metadata == "INCLUDE_ALL_METADATA"
    )
    error_message = "The GKE subnet must emit attributed flow logs with bounded aggregation and sampling."
  }
}
