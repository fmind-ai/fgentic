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

run "accepts_platform_domain" {
  command = plan

  assert {
    condition     = google_dns_record_set.platform["grafana."].name == "grafana.fgentic.example."
    error_message = "Domain validation must preserve the exact derived Grafana DNS record."
  }
}

run "accepts_longest_composable_domain" {
  command = plan

  variables {
    domain = join(".", [
      join("", [for _ in range(63) : "a"]),
      join("", [for _ in range(63) : "b"]),
      join("", [for _ in range(63) : "c"]),
      join("", [for _ in range(53) : "d"]),
    ])
  }

  assert {
    condition     = length(var.domain) == 245 && length(trimsuffix(google_dns_record_set.platform["grafana."].name, ".")) == 253
    error_message = "The longest valid platform apex must produce an exact 253-character Grafana hostname."
  }
}

run "rejects_empty_domain" {
  command = plan

  variables {
    domain = ""
  }

  expect_failures = [var.domain]
}

run "rejects_single_label_domain" {
  command = plan

  variables {
    domain = "localhost"
  }

  expect_failures = [var.domain]
}

run "rejects_domain_too_long_for_derived_hosts" {
  command = plan

  variables {
    domain = join(".", [
      join("", [for _ in range(63) : "a"]),
      join("", [for _ in range(63) : "b"]),
      join("", [for _ in range(63) : "c"]),
      join("", [for _ in range(54) : "d"]),
    ])
  }

  expect_failures = [var.domain]
}

run "rejects_overlong_domain_label" {
  command = plan

  variables {
    domain = "${join("", [for _ in range(64) : "a"])}.example"
  }

  expect_failures = [var.domain]
}

run "rejects_uppercase_domain" {
  command = plan

  variables {
    domain = "Fgentic.example"
  }

  expect_failures = [var.domain]
}

run "rejects_underscore_domain" {
  command = plan

  variables {
    domain = "fgentic_platform.example"
  }

  expect_failures = [var.domain]
}

run "rejects_empty_domain_label" {
  command = plan

  variables {
    domain = "fgentic..example"
  }

  expect_failures = [var.domain]
}

run "rejects_hyphen_prefixed_domain_label" {
  command = plan

  variables {
    domain = "-fgentic.example"
  }

  expect_failures = [var.domain]
}

run "rejects_hyphen_suffixed_domain_label" {
  command = plan

  variables {
    domain = "fgentic-.example"
  }

  expect_failures = [var.domain]
}
