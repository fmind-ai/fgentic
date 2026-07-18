# Enable the required Google APIs declaratively.
locals {
  required_services = [
    "aiplatform.googleapis.com", # Vertex AI — the model behind agentgateway's LLM chokepoint
    "compute.googleapis.com",
    "container.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com", # GKE WIF prerequisite; also used by CNPG's GSA impersonation
    "storage.googleapis.com",        # tfstate + CNPG backup buckets
  ]
  services = concat(local.required_services, var.manage_dns ? ["dns.googleapis.com"] : [])
}

resource "google_project_service" "enabled_services" {
  for_each           = toset(local.services)
  project            = var.project_id
  service            = each.key
  disable_on_destroy = false
}
