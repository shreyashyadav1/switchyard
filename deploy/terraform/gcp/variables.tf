variable "project_id" {
  description = "GCP project to deploy into."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run and Artifact Registry. us-central1, us-east1 and us-west1 are in Cloud Run's free tier."
  type        = string
  default     = "us-central1"
}

variable "service_name" {
  description = "Name of the Cloud Run service, service account and repository."
  type        = string
  default     = "switchyard"
}

variable "image" {
  description = "Container image to run. Leave empty on the first apply, push an image to the repository this module creates, then set it."
  type        = string
  default     = ""
}

variable "config_file" {
  description = "Path to the gateway config (switchyard.yaml). It holds key digests, not secrets, and is stored in Secret Manager so Cloud Run can mount it as a file."
  type        = string
}

variable "provider_key_envs" {
  description = "Environment variables holding provider API keys, matching api_key_env in the config. Terraform creates an empty secret for each; add the value with `gcloud secrets versions add` so it never enters Terraform state."
  type        = set(string)
  default     = ["GROQ_API_KEY"]
}

variable "public" {
  description = "Allow unauthenticated invocations at the Cloud Run layer. The gateway still requires its own API keys."
  type        = bool
  default     = true
}

variable "max_instances" {
  description = "Upper bound on instances. Rate limits and budgets are tracked per instance, so 1 keeps them exact."
  type        = number
  default     = 1
}

variable "otlp_endpoint" {
  description = "Optional OTLP/HTTP endpoint for traces, e.g. a collector or Grafana Cloud. Empty disables tracing."
  type        = string
  default     = ""
}

variable "billing_account" {
  description = "Optional billing account ID. When set, a budget alert is created at budget_usd."
  type        = string
  default     = ""
}

variable "budget_usd" {
  description = "Monthly budget alert threshold in US dollars."
  type        = number
  default     = 1
}
