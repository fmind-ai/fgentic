output "cluster_name" {
  value       = google_container_cluster.cluster.name
  description = "The GKE cluster name"
}

output "gke_connect_command" {
  value       = "gcloud container clusters get-credentials ${google_container_cluster.cluster.name} --location ${local.location} --project ${var.project_id}"
  description = "Command to connect kubectl to the GKE cluster"
}

output "ingress_ip" {
  value       = google_compute_address.ingress_ip.address
  description = "Static IP for the ingress LoadBalancer (DNS A records + Traefik loadBalancerIP)"
}
