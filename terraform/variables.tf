variable "name" {
  description = "Name prefix for all resources."
  type        = string
  default     = "google-m365-sync"
}

variable "lambda_zip" {
  description = "Path to the zip produced by `make zip`."
  type        = string
  default     = "../dist/google-m365-sync.zip"
}

variable "sync_domain" {
  description = "Only users in this domain are synced."
  type        = string
  default     = "corbalt.com"
}

variable "google_admin_subject" {
  description = "Leave empty when the service account is assigned the admin role directly (recommended). Set only for legacy domain-wide delegation."
  type        = string
  default     = ""
}

variable "usage_location" {
  description = "ISO 3166 alpha-2 country set on created users (required before licensing)."
  type        = string
  default     = "US"
}

variable "max_disables" {
  description = "If more Entra users than this lack a Google counterpart, no disables are applied and the run fails."
  type        = number
  default     = 3
}

variable "exclude_upns" {
  description = "UPNs never modified or disabled (break-glass admins, service accounts)."
  type        = list(string)
  default     = []
}

variable "dry_run" {
  description = "Log planned changes without writing to Entra. Start with true."
  type        = bool
}

variable "schedule_expression" {
  description = "EventBridge Scheduler expression (evaluated in America/Los_Angeles)."
  type        = string
  default     = "cron(7 6-19 ? * MON-FRI *)"
}

variable "schedule_enabled" {
  type    = bool
  default = true
}

variable "log_retention_days" {
  type    = number
  default = 365
}

variable "kms_key_arn" {
  description = "Optional customer-managed KMS key for the secrets and log group."
  type        = string
  default     = null
}

variable "alarm_sns_topic_arn" {
  description = "Optional SNS topic notified when a sync run fails."
  type        = string
  default     = null
}
