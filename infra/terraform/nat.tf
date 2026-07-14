# Cloud NAT for the private GKE nodes (they have no public IPs — gke.tf): outbound-only egress
# for image pulls (GHCR) and the agents' LLM traffic through agentgateway.
resource "google_compute_router" "router" {
  name       = "${var.network_name}-router"
  region     = var.region
  network    = google_compute_network.vpc.id
  depends_on = [google_project_service.enabled_services]
}

resource "google_compute_router_nat" "nat" {
  name                               = "${var.network_name}-nat"
  router                             = google_compute_router.router.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}
