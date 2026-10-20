output "repository" {
  description = "Push images here, e.g. <repository>/switchyard:<tag>."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "url" {
  description = "Gateway base URL. Point OpenAI clients at <url>/v1."
  value       = var.image == "" ? null : google_cloud_run_v2_service.gateway[0].uri
}

output "provider_key_secrets" {
  description = "Secrets to fill with `gcloud secrets versions add <name> --data-file=-` before setting var.image."
  value       = { for k, s in google_secret_manager_secret.provider_keys : k => s.secret_id }
}
