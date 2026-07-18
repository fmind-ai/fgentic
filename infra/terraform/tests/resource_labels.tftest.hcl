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

run "labels_reference_cluster" {
  command = plan

  assert {
    condition = google_container_cluster.cluster.resource_labels == tomap({
      application = "fgentic"
      deployment  = "reference"
      managed_by  = "terraform"
    })
    error_message = "The GKE cluster must retain the exact Fgentic reference-deployment ownership labels."
  }
}
