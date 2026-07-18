mock_provider "google" {
  override_during = plan
}

run "bootstrap_accepts_shortest_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "a12345"
    state_bucket_name = "fgentic-test-tfstate"
  }

  assert {
    condition     = google_storage_bucket.tfstate.versioning[0].enabled && google_storage_bucket.tfstate.soft_delete_policy[0].retention_duration_seconds == 604800 && google_storage_bucket.tfstate.public_access_prevention == "enforced"
    error_message = "Bootstrap input validation must preserve the state bucket recovery and public-access controls."
  }
}

run "bootstrap_accepts_longest_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "a12345678901234567890123456789"
    state_bucket_name = "fgentic-test-tfstate"
  }
}

run "bootstrap_rejects_five_character_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "a1234"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}

run "bootstrap_rejects_thirty_one_character_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "a123456789012345678901234567890"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}

run "bootstrap_rejects_uppercase_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "Fgentic-project"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}

run "bootstrap_rejects_digit_prefixed_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "1fgentic-project"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}

run "bootstrap_rejects_hyphen_suffixed_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-project-"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}

run "bootstrap_rejects_underscore_project_id" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic_project"
    state_bucket_name = "fgentic-test-tfstate"
  }

  expect_failures = [var.project_id]
}
