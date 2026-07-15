mock_provider "google" {
  override_during = plan

  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

run "vertex_identity_is_direct_and_scoped" {
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
    condition     = contains(local.services, "iamcredentials.googleapis.com")
    error_message = "The GKE Workload Identity Federation prerequisite API must be enabled."
  }

  assert {
    condition     = google_project_iam_member.agentgateway_vertex_user.project == var.project_id
    error_message = "The Vertex grant must belong to the selected project."
  }

  assert {
    condition     = google_project_iam_member.agentgateway_vertex_user.role == "roles/aiplatform.user"
    error_message = "The agentgateway workload must receive only the Vertex AI User role."
  }

  assert {
    condition     = google_project_iam_member.agentgateway_vertex_user.member == "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/fgentic-test-project.svc.id.goog/subject/ns/agentgateway-system/sa/agentgateway-proxy"
    error_message = "The Vertex grant must target only the generated agentgateway proxy KSA."
  }
}
