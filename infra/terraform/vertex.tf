# agentgateway v1.3.1 names the generated data-plane ServiceAccount after the Gateway:
# `agentgateway-system/agentgateway-proxy`. Grant that Kubernetes principal direct Vertex access;
# no Google service account, impersonation binding, annotation, or key is needed.
data "google_project" "current" {
  project_id = var.project_id
}

resource "google_project_iam_member" "agentgateway_vertex_user" {
  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = "principal://iam.googleapis.com/projects/${data.google_project.current.number}/locations/global/workloadIdentityPools/${google_container_cluster.cluster.workload_identity_config[0].workload_pool}/subject/ns/agentgateway-system/sa/agentgateway-proxy"
}

# A name-based principal is identical across clusters in one project workload pool. The reference
# therefore assumes one trusted Fgentic cluster per project. Shared-project deployments must prevent
# identity sameness, for example with separate projects or a cluster-unique workload identity.
