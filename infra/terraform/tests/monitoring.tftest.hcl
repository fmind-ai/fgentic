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

run "keeps_workload_metrics_inside_the_cluster" {
  command = plan

  assert {
    condition     = google_container_cluster.cluster.monitoring_service == "monitoring.googleapis.com/kubernetes"
    error_message = "The GKE cluster must use the Kubernetes Cloud Monitoring service explicitly."
  }

  assert {
    condition     = length(google_container_cluster.cluster.monitoring_config) == 1 && toset(one(google_container_cluster.cluster.monitoring_config).enable_components) == toset(["SYSTEM_COMPONENTS"])
    error_message = "The GKE cluster must export system metrics only, without workload or optional control-plane metric packages."
  }

  assert {
    condition     = length(one(google_container_cluster.cluster.monitoring_config).managed_prometheus) == 1 && one(one(google_container_cluster.cluster.monitoring_config).managed_prometheus).enabled == false
    error_message = "Google Managed Service for Prometheus must remain disabled in favor of the sovereign in-cluster Prometheus stack."
  }

  assert {
    condition     = contains(keys(google_project_service.enabled_services), "monitoring.googleapis.com")
    error_message = "The Cloud Monitoring API must be part of the required service graph before GKE enables system monitoring."
  }
}
