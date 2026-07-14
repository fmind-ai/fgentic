# GKE Standard cluster sized for the full Matrix + agent stack (zonal by default, regional via
# var.regional for a control-plane SLA). Standard mode (not Autopilot) keeps the node pool
# plain/portable. Workload Identity, Shielded nodes, Dataplane V2 (NetworkPolicy), private
# nodes (egress via Cloud NAT — nat.tf), and a restricted control-plane endpoint are on.
locals {
  location = var.regional ? var.region : var.zone
}

resource "google_container_cluster" "cluster" {
  name     = var.cluster_name
  location = local.location

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = var.deletion_protection

  network    = google_compute_network.vpc.self_link
  subnetwork = google_compute_subnetwork.subnet.self_link

  ip_allocation_policy {
    cluster_secondary_range_name  = "gke-pods"
    services_secondary_range_name = "gke-services"
  }

  # Nodes get no public IPs (SPEC §11): image pulls (GHCR) and LLM egress go through Cloud NAT.
  # The control-plane endpoint stays public but is restricted to master_authorized_networks.
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
  }

  # Weekly patch window outside demo hours (UTC).
  maintenance_policy {
    recurring_window {
      start_time = "2026-01-03T02:00:00Z"
      end_time   = "2026-01-03T08:00:00Z"
      recurrence = "FREQ=WEEKLY;BYDAY=SA"
    }
  }

  # Keyless GCP access for pods that opt in (KSA annotation) — and required for the node pool's
  # GKE_METADATA mode below.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  datapath_provider = "ADVANCED_DATAPATH" # Dataplane V2 (Cilium) => NetworkPolicy enforcement

  release_channel {
    channel = "REGULAR"
  }

  master_authorized_networks_config {
    gcp_public_cidrs_access_enabled = false
    dynamic "cidr_blocks" {
      for_each = var.master_authorized_networks
      content {
        cidr_block   = cidr_blocks.value.cidr_block
        display_name = cidr_blocks.value.display_name
      }
    }
  }

  depends_on = [google_project_service.enabled_services]
}

resource "google_service_account" "gke_node_sa" {
  account_id   = "fgentic-node-sa"
  display_name = "Service Account for Fgentic GKE nodes"
  depends_on   = [google_project_service.enabled_services]
}

resource "google_project_iam_member" "node_log_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.gke_node_sa.email}"
}

resource "google_project_iam_member" "node_metric_writer" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.gke_node_sa.email}"
}

resource "google_container_node_pool" "primary" {
  name       = "primary"
  location   = local.location
  cluster    = google_container_cluster.cluster.name
  node_count = var.node_count # per zone when var.regional is set

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type = var.machine_type
    disk_size_gb = 50
    disk_type    = "pd-balanced" # Synapse + Postgres are latency-sensitive; pd-standard drags
    image_type   = "COS_CONTAINERD"

    service_account = google_service_account.gke_node_sa.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    # Explicit (GKE_METADATA already implies it): pods must not reach the legacy v0.1/v1beta1
    # metadata endpoints, which leak the node token.
    metadata = {
      disable-legacy-endpoints = "true"
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    labels = { pool = "primary" }
  }

  lifecycle {
    ignore_changes = [node_config[0].labels, node_config[0].taint]
  }
}
