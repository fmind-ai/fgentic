mock_provider "google" {
  override_during = plan

  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id             = "fgentic-test-project"
  domain                 = "fgentic.example"
  node_count             = 1
  pg_backups_bucket_name = "fgentic-test-pg-backups"
  master_authorized_networks = [
    {
      cidr_block   = "203.0.113.10/32"
      display_name = "test"
    }
  ]
}

run "defaults_to_six_public_hosts_without_admin" {
  command = plan

  assert {
    condition     = length(google_project_service.enabled_services) == 7 && contains(keys(google_project_service.enabled_services), "dns.googleapis.com")
    error_message = "Managed DNS must enable Cloud DNS in addition to every required platform API."
  }

  assert {
    condition     = google_dns_managed_zone.platform[0].project == google_project_service.enabled_services["dns.googleapis.com"].project
    error_message = "The managed zone must depend on the exact Cloud DNS API service resource."
  }

  assert {
    condition     = length(google_dns_record_set.platform) == 6
    error_message = "Managed DNS must keep the six default platform A records."
  }

  assert {
    condition     = !contains(keys(google_dns_record_set.platform), "admin.")
    error_message = "The admin A record must remain absent by default."
  }
}

run "explicitly_disabled_admin_dns_has_zero_admin_footprint" {
  command = plan

  variables {
    admin_console_dns_enabled = false
  }

  assert {
    condition     = length(google_dns_record_set.platform) == 6 && !contains(keys(google_dns_record_set.platform), "admin.")
    error_message = "Disabling admin-console DNS must retain only the six default platform hosts."
  }
}

run "enabled_admin_dns_adds_exact_a_record" {
  command = plan

  variables {
    admin_console_dns_enabled = true
  }

  assert {
    condition     = length(google_dns_record_set.platform) == 7
    error_message = "Enabling admin-console DNS must add exactly one platform record."
  }

  assert {
    condition     = google_dns_record_set.platform["admin."].name == "admin.fgentic.example."
    error_message = "The opt-in record must target admin.<domain>."
  }

  assert {
    condition     = google_dns_record_set.platform["admin."].type == "A"
    error_message = "The opt-in admin host must be an A record."
  }
}

run "unmanaged_dns_has_zero_records" {
  command = plan

  variables {
    manage_dns = false
  }

  assert {
    condition = toset(keys(google_project_service.enabled_services)) == toset([
      "aiplatform.googleapis.com",
      "compute.googleapis.com",
      "container.googleapis.com",
      "iam.googleapis.com",
      "iamcredentials.googleapis.com",
      "storage.googleapis.com",
    ])
    error_message = "External DNS must retain every required platform API without enabling Cloud DNS."
  }

  assert {
    condition     = length(google_dns_managed_zone.platform) == 0 && length(google_dns_record_set.platform) == 0
    error_message = "Disabling managed DNS must create no Cloud DNS zone or platform records."
  }
}

run "rejects_admin_dns_when_dns_is_unmanaged" {
  command = plan

  variables {
    admin_console_dns_enabled = true
    manage_dns                = false
  }

  expect_failures = [var.admin_console_dns_enabled]
}
