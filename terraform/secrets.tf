# --- Secrets (values are populated out of band so keys never land in state) ---

resource "aws_secretsmanager_secret" "google_sa_key" {
  name        = "${local.name}/google-service-account-key"
  description = "Google Workspace service account JSON key (directory.user.readonly)"
  kms_key_id  = var.kms_key_arn
}

resource "aws_secretsmanager_secret" "entra_cert" {
  name        = "${local.name}/entra-app-certificate"
  description = "PEM: certificate + RSA private key for the Entra app registration"
  kms_key_id  = var.kms_key_arn
}
