# Object store + keyless identity for CloudNativePG backups (SPEC §4 F12): the CNPG Cluster
# (infra/postgres/cluster.yaml) archives WAL + nightly base backups to this bucket via Workload
# Identity — no service-account key is ever created.
resource "google_storage_bucket" "pg_backups" {
  name                        = var.pg_backups_bucket_name
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # Cloud Storage otherwise adds seven days of soft-delete retention to new buckets. Keep the
  # documented 60-day lifecycle rule as the actual hard deletion boundary.
  soft_delete_policy {
    retention_duration_seconds = 0
  }

  lifecycle_rule {
    # CNPG's retentionPolicy (30d) governs catalog retention; this is the belt-and-braces cap.
    condition {
      age = 60
    }
    action {
      type = "Delete"
    }
  }

  depends_on = [google_project_service.enabled_services]
}

resource "google_service_account" "pg_backup" {
  account_id   = "fgentic-pg-backup"
  display_name = "CloudNativePG backups for Fgentic (Workload Identity)"
  depends_on   = [google_project_service.enabled_services]
}

resource "google_storage_bucket_iam_member" "pg_backup_object_admin" {
  bucket = google_storage_bucket.pg_backups.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.pg_backup.email}"
}

# Bind the CNPG cluster's KSA (namespace `postgres`, name = cluster name `platform-pg`) to the
# GSA. The KSA side is annotated in infra/postgres/cluster.yaml (serviceAccountTemplate).
resource "google_service_account_iam_member" "pg_backup_workload_identity" {
  service_account_id = google_service_account.pg_backup.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[postgres/platform-pg]"
}
