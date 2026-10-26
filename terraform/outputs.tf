output "function_name" {
  value = aws_lambda_function.sync.function_name
}

output "google_sa_secret_arn" {
  value = aws_secretsmanager_secret.google_sa_key.arn
}

output "entra_cert_secret_arn" {
  value = aws_secretsmanager_secret.entra_cert.arn
}

output "log_group" {
  value = aws_cloudwatch_log_group.lambda.name
}
