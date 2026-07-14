resource "google_compute_network" "vpc" {
  name                    = var.network_name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.enabled_services]
}

resource "google_compute_subnetwork" "subnet" {
  name                     = "${var.network_name}-subnet"
  ip_cidr_range            = "10.0.0.0/20"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "gke-pods"
    ip_cidr_range = "172.16.0.0/16"
  }
  secondary_ip_range {
    range_name    = "gke-services"
    ip_cidr_range = "10.1.0.0/20"
  }
}

# Static regional external IP for the single ingress LoadBalancer (Traefik pins it via loadBalancerIP).
resource "google_compute_address" "ingress_ip" {
  name       = "fgentic-ingress-ip"
  region     = var.region
  depends_on = [google_project_service.enabled_services]
}
