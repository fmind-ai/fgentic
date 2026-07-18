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

run "accepts_narrow_ipv4_cidr" {
  command = plan
}

run "rejects_canonical_all_ipv4_cidr" {
  command = plan

  variables {
    master_authorized_networks = [
      {
        cidr_block   = "0.0.0.0/0"
        display_name = "all-ipv4"
      }
    ]
  }

  expect_failures = [var.master_authorized_networks]
}

run "rejects_noncanonical_all_ipv4_cidr" {
  command = plan

  variables {
    master_authorized_networks = [
      {
        cidr_block   = "203.0.113.10/0"
        display_name = "all-ipv4-noncanonical"
      }
    ]
  }

  expect_failures = [var.master_authorized_networks]
}

run "rejects_ipv6_cidr" {
  command = plan

  variables {
    master_authorized_networks = [
      {
        cidr_block   = "2001:db8::1/128"
        display_name = "ipv6"
      }
    ]
  }

  expect_failures = [var.master_authorized_networks]
}

run "rejects_invalid_cidr" {
  command = plan

  variables {
    master_authorized_networks = [
      {
        cidr_block   = "not-a-cidr"
        display_name = "invalid"
      }
    ]
  }

  expect_failures = [var.master_authorized_networks]
}
