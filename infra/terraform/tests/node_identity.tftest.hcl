mock_provider "google" {
  override_during = plan

  mock_resource "google_service_account" {
    defaults = {
      email = "fgentic-pg-backup@fgentic-test-project.iam.gserviceaccount.com"
      name  = "projects/fgentic-test-project/serviceAccounts/fgentic-pg-backup@fgentic-test-project.iam.gserviceaccount.com"
    }
  }

  override_resource {
    target = google_service_account.gke_node_sa
    values = {
      email = "fgentic-node-sa@fgentic-test-project.iam.gserviceaccount.com"
      name  = "projects/fgentic-test-project/serviceAccounts/fgentic-node-sa@fgentic-test-project.iam.gserviceaccount.com"
    }
  }

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

run "grants_required_role_before_attaching_node_identity" {
  command = plan

  assert {
    condition     = google_project_iam_member.gke_node_default.project == var.project_id
    error_message = "The GKE node role must be granted in the cluster project."
  }

  assert {
    condition     = google_project_iam_member.gke_node_default.role == "roles/container.defaultNodeServiceAccount"
    error_message = "The custom node identity must receive GKE's complete minimum system-task role."
  }

  assert {
    condition     = google_project_iam_member.gke_node_default.member == "serviceAccount:${google_service_account.gke_node_sa.email}"
    error_message = "The GKE role must bind only the custom node service account."
  }

  assert {
    condition     = google_container_node_pool.primary.node_config[0].service_account == google_service_account.gke_node_sa.email
    error_message = "The node pool must attach the role-bearing custom service account."
  }
}
