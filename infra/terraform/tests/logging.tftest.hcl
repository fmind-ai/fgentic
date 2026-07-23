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

run "keeps_workload_logs_inside_the_cluster" {
  command = plan

  assert {
    condition     = google_container_cluster.cluster.logging_service == "logging.googleapis.com/kubernetes"
    error_message = "The GKE cluster must use the Kubernetes Cloud Logging service explicitly."
  }

  assert {
    condition     = length(google_container_cluster.cluster.logging_config) == 1 && toset(one(google_container_cluster.cluster.logging_config).enable_components) == toset(["SYSTEM_COMPONENTS"])
    error_message = "The GKE cluster must export system logs only, without workload or optional control-plane application logs."
  }

  assert {
    condition     = contains(keys(google_project_service.enabled_services), "logging.googleapis.com")
    error_message = "The Cloud Logging API must be part of the required service graph before GKE enables system logging."
  }
}
