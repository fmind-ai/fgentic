# Enable the required Google APIs declaratively.
locals {
  services = [
    "aiplatform.googleapis.com", # Vertex AI — the model behind agentgateway's LLM chokepoint
    "compute.googleapis.com",
    "container.googleapis.com",
    "dns.googleapis.com", # only exercised when var.manage_dns is set
    "iam.googleapis.com",
    "storage.googleapis.com", # tfstate + CNPG backup buckets
  ]
}

resource "google_project_service" "enabled_services" {
  for_each           = toset(local.services)
  project            = var.project_id
  service            = each.key
  disable_on_destroy = false
}
