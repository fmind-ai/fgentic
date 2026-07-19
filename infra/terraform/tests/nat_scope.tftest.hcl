mock_provider "google" {
  override_during = plan

  mock_resource "google_compute_subnetwork" {
    defaults = {
      id = "projects/fgentic-test-project/regions/europe-west1/subnetworks/fgentic-vpc-subnet"
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

run "scopes_cloud_nat_to_gke_subnet" {
  command = plan

  assert {
    condition     = google_compute_router_nat.nat.source_subnetwork_ip_ranges_to_nat == "LIST_OF_SUBNETWORKS"
    error_message = "Cloud NAT must not grant egress to every present and future subnet."
  }

  assert {
    condition     = length(google_compute_router_nat.nat.subnetwork) == 1 && one(google_compute_router_nat.nat.subnetwork).name == google_compute_subnetwork.subnet.id && toset(one(google_compute_router_nat.nat.subnetwork).source_ip_ranges_to_nat) == toset(["ALL_IP_RANGES"])
    error_message = "Cloud NAT must select every primary and secondary range of the exact GKE subnet."
  }

  assert {
    condition     = google_compute_router_nat.nat.nat_ip_allocate_option == "AUTO_ONLY" && google_compute_router_nat.nat.log_config[0].enable && google_compute_router_nat.nat.log_config[0].filter == "ERRORS_ONLY"
    error_message = "NAT scope changes must preserve automatic IP allocation and error-only logging."
  }
}
