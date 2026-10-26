# --- Schedule ---

data "aws_iam_policy_document" "assume_scheduler" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "scheduler" {
  name               = "${local.name}-scheduler"
  assume_role_policy = data.aws_iam_policy_document.assume_scheduler.json
}

resource "aws_iam_role_policy" "scheduler" {
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.sync.arn
    }]
  })
}

resource "aws_scheduler_schedule" "sync" {
  name                         = local.name
  schedule_expression          = var.schedule_expression
  schedule_expression_timezone = "America/Los_Angeles"
  state                        = var.schedule_enabled ? "ENABLED" : "DISABLED"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.sync.arn
    role_arn = aws_iam_role.scheduler.arn
    input    = "{}"
    retry_policy {
      # Covers only failures to deliver the event; failed runs are handled by
      # aws_lambda_function_event_invoke_config above.
      maximum_retry_attempts = 0
    }
  }
}
