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

run "accepts_shortest_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = "a"
  }

  assert {
    condition     = google_dns_managed_zone.platform[0].name == "a" && google_dns_managed_zone.platform[0].deletion_policy == "PREVENT" && google_dns_managed_zone.platform[0].dnssec_config[0].state == "on"
    error_message = "DNS zone-name validation must preserve the protected DNSSEC-enabled public zone."
  }
}

run "accepts_longest_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = join("", [for _ in range(63) : "a"])
  }
}

run "rejects_empty_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = ""
  }

  expect_failures = [var.dns_zone_name]
}

run "rejects_sixty_four_character_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = join("", [for _ in range(64) : "a"])
  }

  expect_failures = [var.dns_zone_name]
}

run "rejects_uppercase_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = "Fgentic"
  }

  expect_failures = [var.dns_zone_name]
}

run "rejects_digit_prefixed_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = "1fgentic"
  }

  expect_failures = [var.dns_zone_name]
}

run "rejects_hyphen_suffixed_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = "fgentic-"
  }

  expect_failures = [var.dns_zone_name]
}

run "rejects_underscore_dns_zone_name" {
  command = plan

  variables {
    dns_zone_name = "fgentic_zone"
  }

  expect_failures = [var.dns_zone_name]
}
