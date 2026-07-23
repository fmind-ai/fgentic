mock_provider "google" {
  override_during = plan
}

run "bootstrap_accepts_multi_segment_two_digit_region" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    region            = "europe-west12"
    state_bucket_name = "fgentic-test-tfstate"
  }

  assert {
    condition     = google_storage_bucket.tfstate.location == "europe-west12"
    error_message = "Bootstrap region validation must preserve the exact state bucket location."
  }
}

run "bootstrap_rejects_region_without_separator" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    region            = "europewest1"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.region]
}

run "bootstrap_rejects_uppercase_region" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    region            = "EUROPE-WEST1"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.region]
}
