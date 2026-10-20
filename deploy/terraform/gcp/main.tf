locals {
  services = [
    "run.googleapis.com",
    "artifactregistry.googleapis.com",
    "secretmanager.googleapis.com",
  ]
  config_mount = "/etc/switchyard"
}

resource "google_project_service" "enabled" {
  for_each           = toset(local.services)
  service            = each.value
  disable_on_destroy = false
}

resource "google_artifact_registry_repository" "images" {
  repository_id = var.service_name
  location      = var.region
  format        = "DOCKER"
  description   = "Switchyard gateway images"

  # Keep the repository inside the free storage allowance.
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 3
    }
  }
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "604800s"
    }
  }

  depends_on = [google_project_service.enabled]
}

resource "google_service_account" "gateway" {
  account_id   = var.service_name
  display_name = "Switchyard gateway"
}

resource "google_secret_manager_secret" "config" {
  secret_id = "${var.service_name}-config"
  replication {
    auto {}
  }
  depends_on = [google_project_service.enabled]
}

resource "google_secret_manager_secret_version" "config" {
  secret      = google_secret_manager_secret.config.id
  secret_data = file(var.config_file)
}

resource "google_secret_manager_secret" "provider_keys" {
  for_each  = var.provider_key_envs
  secret_id = "${var.service_name}-${lower(replace(each.value, "_", "-"))}"
  replication {
    auto {}
  }
  depends_on = [google_project_service.enabled]
}

resource "google_secret_manager_secret_iam_member" "config" {
  secret_id = google_secret_manager_secret.config.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.gateway.member
}

resource "google_secret_manager_secret_iam_member" "provider_keys" {
  for_each  = google_secret_manager_secret.provider_keys
  secret_id = each.value.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.gateway.member
}

resource "google_cloud_run_v2_service" "gateway" {
  count               = var.image == "" ? 0 : 1
  name                = var.service_name
  location            = var.region
  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  template {
    service_account = google_service_account.gateway.email
    timeout         = "300s"

    scaling {
      min_instance_count = 0
      max_instance_count = var.max_instances
    }

    containers {
      image = var.image
      args  = ["serve", "-config", "${local.config_mount}/switchyard.yaml", "-metrics-listen", ""]

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
        # Bill only while handling requests.
        cpu_idle          = true
        startup_cpu_boost = false
      }

      dynamic "env" {
        for_each = google_secret_manager_secret.provider_keys
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value.secret_id
              version = "latest"
            }
          }
        }
      }

      dynamic "env" {
        for_each = var.otlp_endpoint == "" ? [] : [var.otlp_endpoint]
        content {
          name  = "OTEL_EXPORTER_OTLP_ENDPOINT"
          value = env.value
        }
      }

      volume_mounts {
        name       = "config"
        mount_path = local.config_mount
      }

      startup_probe {
        http_get {
          path = "/healthz"
        }
        period_seconds    = 2
        failure_threshold = 10
      }

      liveness_probe {
        http_get {
          path = "/healthz"
        }
        period_seconds = 30
      }
    }

    volumes {
      name = "config"
      secret {
        secret = google_secret_manager_secret.config.secret_id
        items {
          version = "latest"
          path    = "switchyard.yaml"
        }
      }
    }
  }

  depends_on = [
    google_secret_manager_secret_iam_member.config,
    google_secret_manager_secret_iam_member.provider_keys,
    google_secret_manager_secret_version.config,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "public" {
  count    = var.public && var.image != "" ? 1 : 0
  name     = google_cloud_run_v2_service.gateway[0].name
  location = var.region
  role     = "roles/run.invoker"
  member   = "allUsers"
}

data "google_project" "current" {}

resource "google_billing_budget" "alert" {
  count           = var.billing_account == "" ? 0 : 1
  billing_account = var.billing_account
  display_name    = "${var.service_name} monthly budget"

  budget_filter {
    projects = ["projects/${data.google_project.current.number}"]
  }

  amount {
    specified_amount {
      currency_code = "USD"
      units         = tostring(var.budget_usd)
    }
  }

  threshold_rules {
    threshold_percent = 0.5
  }
  threshold_rules {
    threshold_percent = 1.0
  }
}
