mock_provider "google" {
  override_during = plan
}

run "bootstrap_accepts_shortest_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "a-1"
  }
}

run "bootstrap_accepts_longest_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "a12345678901234567890123456789012345678901234567890123456789012"
  }
}

run "bootstrap_rejects_empty_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = ""
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_short_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "ab"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_long_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "a123456789012345678901234567890123456789012345678901234567890123"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_uppercase_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "Fgentic-tfstate"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_underscore_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "fgentic_tfstate"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_dotted_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "state.example.com"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_hyphen_prefixed_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "-fgentic-tfstate"
  }

  expect_failures = [var.state_bucket_name]
}

run "bootstrap_rejects_hyphen_suffixed_state_bucket_name" {
  command = plan

  module {
    source = "./bootstrap"
  }

  variables {
    project_id        = "fgentic-test-project"
    state_bucket_name = "fgentic-tfstate-"
  }

  expect_failures = [var.state_bucket_name]
}
