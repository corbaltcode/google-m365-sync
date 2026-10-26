# --- IAM ---

data "aws_iam_policy_document" "assume_lambda" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${local.name}-lambda"
  assume_role_policy = data.aws_iam_policy_document.assume_lambda.json
}

data "aws_iam_policy_document" "lambda" {
  statement {
    sid       = "ReadSyncSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.google_sa_key.arn, aws_secretsmanager_secret.entra_cert.arn]
  }

  statement {
    sid     = "ReadEntraParameters"
    actions = ["ssm:GetParameter"]
    resources = [
      "${local.ssm_parameter_arn_prefix}${local.entra_tenant_id_param}",
      "${local.ssm_parameter_arn_prefix}${local.entra_client_id_param}",
    ]
  }

  statement {
    sid       = "WriteLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }

  dynamic "statement" {
    for_each = var.kms_key_arn == null ? [] : [var.kms_key_arn]
    content {
      sid       = "DecryptSecrets"
      actions   = ["kms:Decrypt"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_role_policy" "lambda" {
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}

# --- Function ---

resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${local.name}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn
}

resource "aws_lambda_function" "sync" {
  function_name    = local.name
  role             = aws_iam_role.lambda.arn
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  handler          = "bootstrap"
  filename         = var.lambda_zip
  source_code_hash = filebase64sha256(var.lambda_zip)
  memory_size      = 256
  timeout          = 300

  # One run at a time; overlapping syncs could race on creates.
  reserved_concurrent_executions = 1

  environment {
    variables = {
      SYNC_DOMAIN           = var.sync_domain
      GOOGLE_ADMIN_SUBJECT  = var.google_admin_subject
      GOOGLE_SA_SECRET_ARN  = aws_secretsmanager_secret.google_sa_key.arn
      ENTRA_TENANT_ID_PARAM = local.entra_tenant_id_param
      ENTRA_CLIENT_ID_PARAM = local.entra_client_id_param
      ENTRA_CERT_SECRET_ARN = aws_secretsmanager_secret.entra_cert.arn
      USAGE_LOCATION        = var.usage_location
      MAX_DISABLES          = tostring(var.max_disables)
      EXCLUDE_UPNS          = join(",", var.exclude_upns)
      DRY_RUN               = var.dry_run ? "true" : "false"
    }
  }

  depends_on = [aws_cloudwatch_log_group.lambda, aws_iam_role_policy.lambda]
}

# The scheduler invokes the function asynchronously, so Lambda's own async
# retries (2 by default) would rerun a failed sync. Disable them; the next
# scheduled run is the retry.
resource "aws_lambda_function_event_invoke_config" "sync" {
  function_name          = aws_lambda_function.sync.function_name
  maximum_retry_attempts = 0
}
